package udpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
	"sni/util"
)

type UDPClient struct {
	name string

	conn  *net.UDPConn
	raddr *net.UDPAddr
	laddr *net.UDPAddr

	muteLog bool

	isConnected bool
	isClosed    bool

	closeChannel  chan struct{}
	workerDone    chan struct{}

	queuesLock                  sync.Mutex
	inflight                    []*UDPRequestTracker
	queued                      []*UDPRequestTracker
	presumedDroppedResponses    []*UDPPresumedDropped
	inflightDoorbellRung        bool
	inflightDoorbell            chan struct{}
	udpRetryTimeout             time.Duration
	udpConfidentDroppedTime     time.Duration
	nextTimeout                 time.Time
}

const defaultUDPTries = 3
const defaultUDPRetryTimeout = time.Millisecond*500
const defaultUDPConfidentDroppedTime = time.Second * 5

func NewUDPClient(name string) *UDPClient {
	c := &UDPClient{
		name: name,
		closeChannel: make(chan struct{}, 5),
		workerDone: make(chan struct{}, 1),
		inflightDoorbell: make(chan struct{}, 1),
		udpRetryTimeout: defaultUDPRetryTimeout,
		udpConfidentDroppedTime: defaultUDPConfidentDroppedTime,
	}
	c.MuteLog(false)
	c.log("new %p\n", c)
	go c.worker()
	return c
}

func (c *UDPClient) IsClosed() bool { return c.isClosed }

func (c *UDPClient) MuteLog(muted bool) {
	c.muteLog = muted
}

func (c *UDPClient) RemoteAddress() *net.UDPAddr { return c.raddr }

func (c *UDPClient) SetUDPRetryTimeout(duration time.Duration) { c.udpRetryTimeout = duration }
func (c *UDPClient) SetUDPConfidentDroppedTime(duration time.Duration) { c.udpConfidentDroppedTime = duration }

func (c *UDPClient) IsConnected() bool { return c.isConnected }

func (c *UDPClient) log(fmt string, args ...interface{}) {
	if c.muteLog {
		return
	}
	log.Printf(fmt, args...)
}

func (c *UDPClient) LocalAddr() *net.UDPAddr {
	if c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr().(*net.UDPAddr)
}

func (c *UDPClient) RemoteAddr() *net.UDPAddr {
	if c.conn == nil {
		return nil
	}
	return c.conn.RemoteAddr().(*net.UDPAddr)
}

func (c *UDPClient) SetLocalAddr(addr *net.UDPAddr) {
	c.laddr = addr
}

func (c *UDPClient) Connect(raddr *net.UDPAddr) (err error) {
	c.log("%s: connect to server '%s'\n", c.name, raddr)

	if c.isConnected {
		return fmt.Errorf("%s: already connected to '%s'", c.name, c.raddr)
	}

	c.raddr = raddr

	c.conn, err = net.DialUDP("udp", c.laddr, raddr)
	if err != nil {
		return
	}

	c.isConnected = true
	c.log("%s: UDP pipe opened to raddr '%s'\n", c.name, raddr)

	return
}

func (c *UDPClient) Close() error {
	if c.isClosed {
		return nil
	}

	c.isClosed = true
	c.isConnected = false
	c.closeChannel <- struct{}{}

	// do cleanup asynchronously
	go func() {
		if c.conn != nil {
			c.conn.Close()
		}
		<-c.workerDone
		c.conn = nil

		// complete all outstanding requests
		c.queuesLock.Lock()
		defer c.queuesLock.Unlock()
		for _, rt := range c.inflight {
			rt.complete <- UDPRequestComplete{
				Error: fmt.Errorf("UDPClient has been closed"),
				Answer: nil,
				Request: rt.request,
			}
		}
		for _, rt := range c.queued {
			rt.complete <- UDPRequestComplete{
				Error: fmt.Errorf("UDPClient has been closed"),
				Answer: nil,
				Request: rt.request,
			}
		}
	}()

	return nil
}

type UDPRequestExpectingResponse struct {
	UDPToSend             []byte
	ResponseMustBeginWith []byte
	Retryable             bool
}

type UDPRequestComplete struct {
	Error   error
	Answer  []byte
	Request *UDPRequestExpectingResponse
}

func (c *UDPClient) SendMessageNowExpectingNoResponse(message []byte) error {
	if c.isClosed || !c.isConnected {
		return fmt.Errorf("UDPClient has not connected or is closed")
	} else {
		_, err := c.conn.Write(message)
		return err
	}
}

func (c *UDPClient) StartRequest(ctx context.Context, request *UDPRequestExpectingResponse) chan UDPRequestComplete {
	c.MuteLog(false)
	c.log("startreq %p\n", c)
	if c.isClosed || !c.isConnected {
		tempch := make(chan UDPRequestComplete, 1)
		tempch <- UDPRequestComplete{
			Error: fmt.Errorf("UDPClient has not connected or is closed"),
			Answer: nil,
			Request: request,
		}
		return tempch
	}

	rt := &UDPRequestTracker{ctx: ctx,
						request: request,
						complete: make(chan UDPRequestComplete, 1),
						nextTimeout: time.Now(), // will be overwritten before using
						triesFailed: 0}

	conflictDetected := false
	c.queuesLock.Lock()
	{
		for _, presumedDropped := range c.presumedDroppedResponses {
			if c.hasAmbiguity2(rt, presumedDropped) {
				conflictDetected = true
				break
			}
		}
		if !conflictDetected {
			for _, inflightReq := range c.inflight {
				if c.hasAmbiguity(rt, inflightReq) {
					conflictDetected = true
					break
				}
			}
		}
		if !conflictDetected {
			// don't let the new request leapfrog any conflicting requests waiting their turn, as it
			// would delay the waiting request
			for _, queuedReq := range c.queued {
				if c.hasAmbiguity(rt, queuedReq) {
					conflictDetected = true
					break
				}
			}
		}

		if conflictDetected {
			c.queued = append(c.queued, rt)
			// queued can be put inflight when another request completes or times out
		} else {
			c.inflight = append(c.inflight, rt)
			// TODO: should non- Retryable requests get a longer timeout?
			rt.nextTimeout = time.Now().Add(c.udpRetryTimeout)
			// signal that there's work to do
			if !c.inflightDoorbellRung {
				c.inflightDoorbell <- struct{}{}
			}
		}
	}
	c.queuesLock.Unlock()

	if !conflictDetected {
		// request is ok to send and has been marked 'inflight'. send it now
		c.sendRequest_NoLockHeld(rt)
	}
	return rt.complete
}

// internal:

type UDPRequestTracker struct {
	ctx          context.Context
	request      *UDPRequestExpectingResponse
	complete     chan UDPRequestComplete
	nextTimeout  time.Time
	triesFailed  int
}

type UDPPresumedDropped struct {
	ResponseMustBeginWith  []byte
	forgetAfter            time.Time
	maxCount               int
}

func (c *UDPClient) hasAmbiguity(request1 *UDPRequestTracker, request2 *UDPRequestTracker) bool {
    // if both requests have the same expected response prefix, or one expected response prefix
    // is a shortened version of the other one, they cannot both be inflight at the same time
    if bytes.HasPrefix(request1.request.ResponseMustBeginWith, request2.request.ResponseMustBeginWith) ||
       bytes.HasPrefix(request2.request.ResponseMustBeginWith, request1.request.ResponseMustBeginWith) {

       return true
    }
    return false
}
func (c *UDPClient) hasAmbiguity2(request1 *UDPRequestTracker, request2 *UDPPresumedDropped) bool {
    if bytes.HasPrefix(request1.request.ResponseMustBeginWith,         request2.ResponseMustBeginWith) ||
       bytes.HasPrefix(        request2.ResponseMustBeginWith, request1.request.ResponseMustBeginWith) {

       return true
    }
    return false
}

// caller must ensure request is already stored in the c.inflight object
// on send failure, we don't retry. request is pulled out of c.inflight and completed to sender (thus no return value)
func (c *UDPClient) sendRequest_NoLockHeld(rt *UDPRequestTracker) {

	_, err := c.conn.Write([]byte(rt.request.UDPToSend))

	if err != nil {
		c.queuesLock.Lock()
		{
			for i, inflightReq := range c.inflight {
				if inflightReq == rt {
					c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
					break
				}
			}
		}
		c.queuesLock.Unlock()

		rt.complete <- UDPRequestComplete{
			Error: err,
			Answer: nil,
			Request: rt.request,
		}
	}
}

func (c *UDPClient) worker() {
	defer util.Recover()
	var err error
	var rt *UDPRequestTracker
	c.log("worker %p\n", c)

	for {
		var doSocketRead bool
		var inflightRequestsToWrite []*UDPRequestTracker

		c.queuesLock.Lock()
		c.inflightDoorbellRung = false
		if len(c.presumedDroppedResponses) > 0 || len(c.inflight) > 0 || len(c.queued) > 0 {
			// refresh the data that we manage in case we can get rid of some for being out of date,
			// or need to do a retry, or need to send a new request from the queue
			for i := len(c.presumedDroppedResponses)-1; i >= 0; i-- {
				// the originally-expected packet should have never been this delayed, so start
				// accepting the inflighting of similar requests
				if time.Now().After(c.presumedDroppedResponses[i].forgetAfter) {
					c.presumedDroppedResponses = append(c.presumedDroppedResponses[:i],
					    c.presumedDroppedResponses[i+1:]...)
				}
			}
			for i := len(c.inflight)-1; i >= 0; i-- {
				rt = c.inflight[i]
				if rt.ctx.Err() != nil {
					// sender doesn't want the request anymore
					c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
					rt.complete <- UDPRequestComplete{
						Error: rt.ctx.Err(),
						Answer: nil,
						Request: rt.request,
					}
				}
				if time.Now().After(rt.nextTimeout) {
					rt.triesFailed++
					if rt.triesFailed >= defaultUDPTries || !rt.request.Retryable {
						c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
						c.presumedDroppedResponses = append(c.presumedDroppedResponses, &UDPPresumedDropped{
							ResponseMustBeginWith: rt.request.ResponseMustBeginWith,
							forgetAfter: time.Now().Add(c.udpConfidentDroppedTime),
							maxCount: rt.triesFailed,
						})
						rt.complete <- UDPRequestComplete{
							Error: fmt.Errorf("Request did not get a response within the number of retries"),
							Answer: nil,
							Request: rt.request,
						}
					} else {
						// retry
						rt.nextTimeout = time.Now().Add(c.udpRetryTimeout)
						inflightRequestsToWrite = append(inflightRequestsToWrite, rt)
					}
				}
			}
			for i := len(c.queued)-1; i >= 0; i-- {
				rt = c.queued[i]
				if rt.ctx.Err() != nil {
					// sender doesn't want the request anymore
					c.queued = append(c.queued[:i], c.queued[i+1:]...)
					rt.complete <- UDPRequestComplete{
						Error: rt.ctx.Err(),
						Answer: nil,
						Request: rt.request,
					}
				}
			}
		}
		conflictDetected := false
		for !conflictDetected && len(c.queued) != 0 {
			// prepare to send off the next queued requests if they no longer conflict
			// (happens when the request(s) it was conflicting with have completed or been forgotten).
			// do not read past queued[0] at any time so as to prevent a request from getting
			// permanently stuck by a narrower request leapfrogging it repeatedly
			for _, presumedDropped := range c.presumedDroppedResponses {
				if c.hasAmbiguity2(c.queued[0], presumedDropped) {
					conflictDetected = true
					break
				}
			}
			if !conflictDetected {
				for _, inflightReq := range c.inflight {
					if c.hasAmbiguity(c.queued[0], inflightReq) {
						conflictDetected = true
						break
					}
				}
			}
			if !conflictDetected {
				rt, c.queued = c.queued[0], c.queued[1:]
				c.inflight = append(c.inflight, rt)
				inflightRequestsToWrite = append(inflightRequestsToWrite, rt)
				rt.nextTimeout = time.Now().Add(c.udpRetryTimeout)
			}
		}
		if len(c.inflight) > 0 || len(c.presumedDroppedResponses) > 0 {

			// find the min time across the time that each thing is expected to happen
			var nextTimeout time.Time
			if len(c.inflight) > 0 {
				nextTimeout = c.inflight[0].nextTimeout
			} else {
				nextTimeout = c.presumedDroppedResponses[0].forgetAfter
			}
			for _, rt := range c.inflight {
				if rt.nextTimeout.Before(nextTimeout) {
					nextTimeout = rt.nextTimeout
				}
			}
			for _, presumedDropped := range c.presumedDroppedResponses {
				if presumedDropped.forgetAfter.Before(nextTimeout) {
					nextTimeout = presumedDropped.forgetAfter
				}
			}
			c.nextTimeout = nextTimeout

			err = c.conn.SetReadDeadline(c.nextTimeout)

			if err != nil {
				panic(err)
			}

			doSocketRead = true
		} else {
			doSocketRead = false
		}

		c.queuesLock.Unlock()

		for _, rt = range inflightRequestsToWrite {
			c.sendRequest_NoLockHeld(rt)
		}

		if doSocketRead {
			var n int
			b := make([]byte, 65536)
			n, err = c.conn.Read(b)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					err = nil // this was a general deadline, we'll check requests' individual timeouts next iteration
				} else if c.isClosed {
					err = nil // move on to cleanup
				} else {
					panic(err) // anything else recoverable? TODO this should close the class, not the app
				}
			}
			b = b[:n]

			c.queuesLock.Lock()
			foundDroppedPacket := false
			for i, presumedDropped := range c.presumedDroppedResponses {
				if bytes.HasPrefix(b, presumedDropped.ResponseMustBeginWith) {
					presumedDropped.maxCount--
					if presumedDropped.maxCount == 0 {
						c.presumedDroppedResponses = append(c.presumedDroppedResponses[:i],
							c.presumedDroppedResponses[i+1:]...)
					}
					foundDroppedPacket = true // wasn't so dropped after all
					break
				}
			}
			var i int
			if !foundDroppedPacket {
				for i, rt = range c.inflight {
					if bytes.HasPrefix(b, rt.request.ResponseMustBeginWith) {
						c.inflight = append(c.inflight[:i], c.inflight[i+1:]...)
						if rt.triesFailed != 0 {
							// we sent this request more than once, so more responses might come in
							c.presumedDroppedResponses = append(c.presumedDroppedResponses, &UDPPresumedDropped{
								ResponseMustBeginWith: rt.request.ResponseMustBeginWith,
								forgetAfter: time.Now().Add(c.udpConfidentDroppedTime),
								maxCount: rt.triesFailed,
							})
						}
						// complete the request successfully!
						rt.complete <- UDPRequestComplete{
							Error: nil,
							Answer: b,
							Request: rt.request,
						}
						break
					}
				}
			}
			c.queuesLock.Unlock()

			// check for closure
			select {
			case <-c.closeChannel:
				c.workerDone <- struct{}{}
				return
			default:
			}
		} else {

			// check for closure and sleep until a new request is inflight
			select {
			case <-c.closeChannel:
				c.workerDone <- struct{}{}
				return
			case <-c.inflightDoorbell:
			}
		}
	}
}

// TODO remember to strings.TrimSpace()
