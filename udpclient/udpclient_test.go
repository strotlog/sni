package udpclient

import (
	"bytes"
	"context"
	"errors"
	//"fmt"
	//"log"
	"net"
	"reflect"
	"runtime/debug"
	//"sync"
	"testing"
	"time"
)

func createConnection(t *testing.T) (server net.PacketConn, client *UDPClient) {
	server, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("net.ListenPacket failed: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("net.DialUDP failed: %v", err)
	}
	client = NewUDPClient("test", conn)
	return
}

func clientSendRaw(
	t *testing.T,
	server net.PacketConn,
	client *UDPClient,
	message []byte,
) {
	client.SendMessageNowExpectingNoResponse(message)

	_ = serverAssertReceived(t, server, message)
}

func clientSendRequest_AssertArrivesImmediately(
	t *testing.T,
	server net.PacketConn,
	client *UDPClient,
	req *UDPRequestExpectingResponse,
) (completionChan chan UDPRequestComplete, replyTo net.Addr) {

	completionChan = client.StartRequest(context.Background(), req)

	replyTo = serverAssertReceived(t, server, req.UDPToSend)
	return
}

func serverAssertReceived(t *testing.T, server net.PacketConn, message []byte) net.Addr {
	server.SetReadDeadline(time.Now().Add(time.Millisecond * 15))
	recvd := make([]byte, 65536)
	n, replyTo, err := server.ReadFrom(recvd)
	if err != nil {
		t.Log(string(debug.Stack()))
		t.Fatalf("server PacketConn.ReadFrom expected immediate return and failed: %v", err)
	}
	recvd = recvd[:n]
	if !bytes.Equal(message, recvd) {
		t.Log(string(debug.Stack()))
		t.Fatalf("client should have sent '%s' but server received '%s'", string(message), string(recvd))
	}
	return replyTo
}

func assertServerReceivedNoFurtherData(t *testing.T, server net.PacketConn) {
	server.SetReadDeadline(time.Now().Add(time.Millisecond * 30))
	recvd := make([]byte, 65536)
	n, _, err := server.ReadFrom(recvd)
	if err == nil {
		t.Log(string(debug.Stack()))
		t.Fatalf("server received '%s' but should have received nothing from UDPClient", string(recvd[:n]))
	}
	if err2, ok := err.(net.Error); !ok || !err2.Timeout() {
		t.Log(string(debug.Stack()))
		t.Fatalf("expected server read timeout but got error: %v", err)
	}
}

func assert1ChannelReceivedReply(
	t *testing.T,
	assertRepliedChan chan UDPRequestComplete,
	assertReplyData []byte,
	assertNoDataChans []chan UDPRequestComplete,
) {
	assertNoChannelReceivedReply(t, assertNoDataChans)

	var completion UDPRequestComplete

	select {
	case completion = <-assertRepliedChan:
	case <-time.After(time.Millisecond * 15):
		t.Log(string(debug.Stack()))
		t.Fatalf("request didn't receive expected reply '%s'", string(assertReplyData))
	}
	if completion.Error != nil {
		t.Log(string(debug.Stack()))
		t.Fatalf("reply retrieval failed: %v", completion.Error)
	}
	if !bytes.Equal(completion.Answer, assertReplyData) {
		t.Log(string(debug.Stack()))
		t.Fatalf("server should have sent '%s' but client received response '%s'", string(assertReplyData), string(completion.Answer))
	}

	assertNoChannelReceivedReply(t, assertNoDataChans)
}

func assertNoChannelReceivedReply(t *testing.T, assertNoDataChans []chan UDPRequestComplete) {
	time.Sleep(time.Millisecond)

	var cases []reflect.SelectCase
	for _, ch := range assertNoDataChans {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
	}
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectDefault})
	chosen, recv, _ := reflect.Select(cases)

	if chosen != len(cases)-1 {
		t.Log(string(debug.Stack()))
		t.Fatalf("channel index %d among assertNoDataChans received a UDP completion: '%s'", chosen, string(recv.Interface().(UDPRequestComplete).Answer))
	}
}

func Test_Send(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	clientSendRaw(t, server, client, []byte("Test_Send 1"))
	clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("Test_Send 2"),
		ResponseMustBeginWith: []byte(""),
		Retryable:             false,
	})
}

func Test_SendRecv(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	var completionChan chan UDPRequestComplete
	var replyTo net.Addr

	completionChan, replyTo = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("Test_SendRecv 1"),
		ResponseMustBeginWith: []byte(""),
		Retryable:             false,
	})
	server.WriteTo([]byte("anything"), replyTo)

	assert1ChannelReceivedReply(t, completionChan, []byte("anything"), []chan UDPRequestComplete{})

	// re-test with a narrower server response in mind
	completionChan, replyTo = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("Test_SendRecv 2"),
		ResponseMustBeginWith: []byte("hi ba"),
		Retryable:             false,
	})
	server.WriteTo([]byte("hi back"), replyTo)

	assert1ChannelReceivedReply(t, completionChan, []byte("hi back"), []chan UDPRequestComplete{})

	// re-test with a too-narrow server response in mind (supposed to fail)
	completionChan, replyTo = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("Test_SendRecv 3"),
		ResponseMustBeginWith: []byte("hi but too specific"),
		Retryable:             false,
	})
	t.Logf("test udp server: sending invalid response to UDPClient")
	server.WriteTo([]byte("hi"), replyTo)

	// wait extra time just to be sure
	time.Sleep(time.Millisecond * 30)
	assertNoChannelReceivedReply(t, []chan UDPRequestComplete{completionChan})
}

func Test_OutOfOrderCompletion(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	var completionChan1, completionChan2 chan UDPRequestComplete
	var replyTo net.Addr

	clientsend1 := []byte("complete me later: request")
	clientsend2 := []byte("complete me first: request")
	respA := []byte("complete me first: response")
	respB := []byte("complete me later: response")
	completionChan1, replyTo = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             clientsend1,
		ResponseMustBeginWith: clientsend1[:16],
		Retryable:             false,
	})
	completionChan2, _ = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             clientsend2,
		ResponseMustBeginWith: clientsend2[:16],
		Retryable:             false,
	})
	// make server reply to 2nd req first
	server.WriteTo(respA, replyTo)
	assert1ChannelReceivedReply(t, completionChan2, respA, []chan UDPRequestComplete{completionChan1})

	// now server replies to 1st req
	server.WriteTo(respB, replyTo)
	assert1ChannelReceivedReply(t, completionChan1, respB, []chan UDPRequestComplete{})
}

func Test_RequestCollision(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	var completionChan1, completionChan2, completionChan3, completionChan4, completionChan5 chan UDPRequestComplete
	var replyTo net.Addr

	completionChan1, replyTo = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req1"),
		ResponseMustBeginWith: []byte("sameprefix"),
		Retryable:             false,
	})
	completionChan2, _ = clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req2"),
		ResponseMustBeginWith: []byte("differentprefix"),
		Retryable:             false,
	})

	completionChan3 = client.StartRequest(context.Background(), &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req3"),
		ResponseMustBeginWith: []byte("sameprefix"),
		Retryable:             false,
	})
	completionChan4 = client.StartRequest(context.Background(), &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req4"),
		ResponseMustBeginWith: []byte("samepr"),
		Retryable:             false,
	})
	completionChan5 = client.StartRequest(context.Background(), &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req5"),
		ResponseMustBeginWith: []byte("sameprefixlonger"),
		Retryable:             false,
	})
	assertServerReceivedNoFurtherData(t, server) // assert that only req1 and req2 went on the wire

	resp1 := []byte("sameprefix 1")
	resp2 := []byte("differentprefix 2")
	resp3 := []byte("sameprefix 3")

	server.WriteTo(resp1, replyTo)
	// this write causes the UDPClient to do 2 things: 1) completes req1 back to us; 2) inflights req3 i.e. puts it on the wire
	_ = serverAssertReceived(t, server, []byte("req3"))
	assert1ChannelReceivedReply(t, completionChan1, resp1, []chan UDPRequestComplete{completionChan2, completionChan3, completionChan4, completionChan5})

	server.WriteTo(resp2, replyTo)
	assert1ChannelReceivedReply(t, completionChan2, resp2, []chan UDPRequestComplete{completionChan3, completionChan4, completionChan5})

	server.WriteTo(resp3, replyTo)
	assert1ChannelReceivedReply(t, completionChan3, resp3, []chan UDPRequestComplete{completionChan4, completionChan5})
}

func testCancelHelper(t *testing.T, server net.PacketConn, client *UDPClient) {
	var completionChan1, completionChan2, completionChan3 chan UDPRequestComplete
	var completion UDPRequestComplete
	var ctx context.Context
	var cancel context.CancelFunc

	// the next request is the longest-running test in the file in 2022, at about 500 milliseconds when successful,
	// because UDPClient doesn't poll more finely than that for cancellations by default
	ctx, cancel = context.WithCancel(context.Background())

	completionChan1 = client.StartRequest(ctx, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req1"),
		ResponseMustBeginWith: []byte("rsp1"),
		Retryable:             false,
	})
	cancel()
	select {
	case completion = <-completionChan1:
	case <-time.After(time.Second * 6):
		t.Fatalf("request did not cancel after 6 seconds")
	}
	if !errors.Is(completion.Error, context.Canceled) {
		t.Fatalf("canceled request failed with other error: %v", completion.Error)
	}

	// repeat but canceling before even handing off the request
	ctx, cancel = context.WithCancel(context.Background())

	cancel()
	completionChan2 = client.StartRequest(ctx, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req2"),
		ResponseMustBeginWith: []byte("rsp2"),
		Retryable:             false,
	})
	select {
	case completion = <-completionChan2:
	case <-time.After(time.Millisecond * 15):
		t.Fatalf("early canceled request did not come back quickly as canceled")
	}
	if !errors.Is(completion.Error, context.Canceled) {
		t.Fatalf("canceled request failed with other error: %v", completion.Error)
	}

	// repeat but with a timeout (equivalent to deadline) instead of a cancel call
	ctx, _ = context.WithTimeout(context.Background(), time.Millisecond*20)
	completionChan3 = client.StartRequest(ctx, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req3"),
		ResponseMustBeginWith: []byte("rsp3"),
		Retryable:             false,
	})
	select {
	case completion = <-completionChan3:
	case <-time.After(time.Millisecond * 50):
		t.Fatalf("quick timeout request did not come back quickly as timed out")
	}
	if err, ok := completion.Error.(net.Error); !ok || !err.Timeout() {
		t.Fatalf("timed out request failed with other error: %v", completion.Error)
	}
}

func Test_Cancel(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	testCancelHelper(t, server, client)

	// since the last request went on the wire but we got it back long before before UDPClient truly
	// decided to forget about it, also verify that a conflicting request does not go on the wire
	// clear out server receives
	server.SetReadDeadline(time.Now().Add(time.Millisecond * 2))
	for {
		recvd := make([]byte, 65536)
		_, _, err := server.ReadFrom(recvd)
		if err != nil {
			break
		}
	}

	completionChan4 := client.StartRequest(context.Background(), &UDPRequestExpectingResponse{
		UDPToSend:             []byte("req4"),
		ResponseMustBeginWith: []byte("rsp3"),
		Retryable:             false,
	})
	assertNoChannelReceivedReply(t, []chan UDPRequestComplete{completionChan4})
	assertServerReceivedNoFurtherData(t, server)
}

func Test_CancelFromQueue(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	// repeat Test_Cancel(), but this time with an extra inflight request that comes first, blocking
	// the test's requests from being inflighted (i.e., being put on the wire)
	completionChan, replyTo := clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("Test_CancelFromQueue"),
		ResponseMustBeginWith: []byte(""),
		Retryable:             true,
	})

	testCancelHelper(t, server, client)

	assertNoChannelReceivedReply(t, []chan UDPRequestComplete{completionChan})
	// verify that nothing was sent to server
	assertServerReceivedNoFurtherData(t, server)
	// also verify that our original request still functions after having started+canceled others
	server.WriteTo([]byte("resp"), replyTo)
	assert1ChannelReceivedReply(t, completionChan, []byte("resp"), []chan UDPRequestComplete{})
}

func testDelayedOrDroppedMessage(t *testing.T, server net.PacketConn, client *UDPClient, dropMessage bool) {

	client.SetUDPRetryTimeout(time.Millisecond * 20)
	client.SetUDPConfidentDroppedTime(time.Millisecond * 70) // 50 thru 95ms get both callers' tests to pass

	// we're asserting that the first request arrives at the 'server' on our real and reliable localhost connection,
	// but simulating that it is either never seen by a real server OR the real server's reply is lost (which are
	// equivalent for a retryable request)
	completionChan, replyTo := clientSendRequest_AssertArrivesImmediately(t, server, client, &UDPRequestExpectingResponse{
		UDPToSend:             []byte("testDelayedOrDroppedMessage"),
		ResponseMustBeginWith: []byte(""),
		Retryable:             true,
	})
	time.Sleep(time.Millisecond * 21)

	// assert the *retry* also arrived
	serverAssertReceived(t, server, []byte("testDelayedOrDroppedMessage"))
	// and only now respond
	server.WriteTo([]byte("anything"), replyTo)
	time.Sleep(time.Millisecond)
	select {
	case <-completionChan:
	default:
		t.Fatalf("request did not receive the response that was sent")
	}

	// assert no more retries were done
	server.SetReadDeadline(time.Now().Add(time.Millisecond))
	recvd := make([]byte, 65536)
	_, _, err := server.ReadFrom(recvd)
	if err2, ok := err.(net.Error); err == nil || !ok || !err2.Timeout() {
		t.Fatalf("server received more than 2 messages, should have been 2; ReadFrom returned err: %v", err)
	}

	completionChan2 := client.StartRequest(context.Background(), &UDPRequestExpectingResponse{
		UDPToSend:             []byte("testDelayedOrDroppedMessage 2"),
		ResponseMustBeginWith: []byte(""),
		Retryable:             true,
	})

	// UDPClient should be busy waiting a while longer for its 2nd message (the original retry) to possibly get a response, and NOT put our latest request on the wire
	server.SetReadDeadline(time.Now().Add(time.Millisecond))
	recvd = make([]byte, 65536)
	_, _, err = server.ReadFrom(recvd)
	if err2, ok := err.(net.Error); err == nil || !ok || !err2.Timeout() {
		t.Fatalf("server received more than 2 messages, should have been 2; ReadFrom returned err: %v", err)
	}
	assertNoChannelReceivedReply(t, []chan UDPRequestComplete{completionChan2})

	if dropMessage {
		// just wait a little while and the queued message should inflight
		server.SetReadDeadline(time.Now().Add(time.Millisecond * 70))
		recvd = make([]byte, 65536)
		n, _, err := server.ReadFrom(recvd)
		if err != nil {
			t.Fatalf("with packet loss, expected UDPClient to unstuck its next request after ConfidentDroppedTime, but server read returned: %v", err)
		}
		recvd = recvd[:n]
		if !bytes.Equal(recvd, []byte("testDelayedOrDroppedMessage 2")) {
			t.Fatalf("server received '%s' instead of 'testDelayedOrDroppedMessage 2", string(recvd))
		}
	} else {
		// respond to the original request's retry. UDPClient should DROP this response, but also initiate the queued request
		server.WriteTo([]byte("anything"), replyTo)
		serverAssertReceived(t, server, []byte("testDelayedOrDroppedMessage 2"))
		assertNoChannelReceivedReply(t, []chan UDPRequestComplete{completionChan2})
	}

	// server responds to the 2nd true request as normal
	server.WriteTo([]byte("anything"), replyTo)
	assert1ChannelReceivedReply(t, completionChan2, []byte("anything"), []chan UDPRequestComplete{})

	// nothing more should be sent to server
	server.SetReadDeadline(time.Now().Add(time.Millisecond))
	recvd = make([]byte, 65536)
	_, _, err = server.ReadFrom(recvd)
	if err2, ok := err.(net.Error); err == nil || !ok || !err2.Timeout() {
		t.Fatalf("server received more than 3 messages, should have been 3; ReadFrom returned err: %v", err)
	}
}

func Test_RespondAfterRetryInterval(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	testDelayedOrDroppedMessage(t, server, client, false)
}

func Test_DroppedMessage(t *testing.T) {
	server, client := createConnection(t)
	defer server.Close()
	defer client.Close()

	testDelayedOrDroppedMessage(t, server, client, true)
}
