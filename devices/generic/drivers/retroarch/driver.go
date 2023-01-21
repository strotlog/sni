package retroarch

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"net/url"
	"reflect"
	"sni/devices"
	"sni/devices/snes/drivers/retroarch"
	"sni/protos/sni"
	"sni/util"
	"sni/util/env"
	"strings"
	"sync"
	"time"
)

const driverName = "ra"

var logDetector = false
var driver *Driver
var detectInterval = time.Second * 2

type Driver struct {
	container devices.DeviceContainer

	localUdpPortsToAvoid map[int]struct{}

	detectorsCloser  chan struct{} // TODO if we ever get a close signal, call close(detectorsCloser) to stop the detectors
	detectorsLock     sync.RWMutex  // inverted meaning: RLock() to write own entry, Lock() to read all entries
	recentDetections  []recentDetection
	detectorAddrs     []*net.UDPAddr
	detectoredClients [][]*retroarch.RAClient
}

var udpSizes = []int{
	508, // constrained network: low end
	576, // constrained network
	1023, // RA ~1.11.0 internal receive buffer size
	1400, // normal network: low end
	1480, // normal network
	2047, // RA 1.14.0 internal receive buffer size (increased as of this version)
	8900, // jumbo frame network: low end
	65000, // localhost or network capable of fragmentation
}

const maxUdpPacketToTrySending = 2048

type recentDetection struct {
	raVersion                   string
	raVersionReceivedAt         time.Time
	system                      string
	systemReceivedAt            time.Time
	udpMaxSendSize              int
	udpMaxRecvSize              int
	udpSizeDetectStateChangedAt time.Time
	udpSizeDetectStateActive    bool
	udpSendSizeOk               map[int]struct{}
	udpRecvSizeOk               map[int]struct{}
}

func NewDriver(addresses []*net.UDPAddr) *Driver {
	d := &Driver{
		localUdpPortsToAvoid: make(map[int]struct{}),
		detectorsCloser:      make(chan struct{}),
		recentDetections:     make([]recentDetection, len(addresses)),
		detectorAddrs:        addresses,
		detectoredClients:    make([][]*retroarch.RAClient, len(addresses)),
	}

	// retroarch addresses are often on the local machine, so do not bind on any of their port numbers
	for _, addr := range addresses {
		d.localUdpPortsToAvoid[addr.Port] = struct{}{}
	}

	d.container = devices.NewDeviceDriverContainer(d.openDevice)

	d.startDetectors(addresses)

	return d
}

func (d *Driver) DisplayOrder() int {
	return 1
}

func (d *Driver) DisplayName() string {
	return "RetroArch"
}

func (d *Driver) DisplayDescription() string {
	return "Connect to a RetroArch emulator"
}

func (d *Driver) Kind() string { return "retroarch" }

// TODO: sni.DeviceCapability_ExecuteASM
var driverCapabilities = []sni.DeviceCapability{
	sni.DeviceCapability_ReadMemory,
	sni.DeviceCapability_WriteMemory,
	sni.DeviceCapability_ResetSystem,
	sni.DeviceCapability_PauseToggleEmulation,
	sni.DeviceCapability_FetchFields,
}

func (d *Driver) HasCapabilities(capabilities ...sni.DeviceCapability) (bool, error) {
	return devices.CheckCapabilities(capabilities, driverCapabilities)
}

func (d *Driver) openDevice(uri *url.URL) (q devices.Device, err error) {
	// create a new device with its own connection:
	var addr *net.UDPAddr
	addr, err = net.ResolveUDPAddr("udp", uri.Host)
	if err != nil {
		return
	}

	conn, err := d.dialUDPWithGoodLocalPort(addr)
	if err != nil {
		return
	}
	var c *retroarch.RAClient
	c = retroarch.NewRAClient(addr.String(), conn, time.Second*5)

	c.MuteLog(false)

	// detectors may be accessing the same device (even if on a different udp connection) as the one we're sharing,
	// so store a pointer to (non-closed) devices
	d.detectorsLock.Lock()
	defer d.detectorsLock.Unlock()
	detectorFound := false
	requiredRecency := time.Now().Add(-time.Second * 10) // a bit more leeway here for simple matching than in Detect()
	for i, detectorAddr := range d.detectorAddrs {
		// prune closed
		for j := len(d.detectoredClients[i]) - 1; j >= 0; j-- {
			if d.detectoredClients[i][j].IsClosed() {
				d.detectoredClients[i] = append(d.detectoredClients[i][:j], d.detectoredClients[i][j+1:]...)
			}
		}
		if reflect.DeepEqual(addr, detectorAddr) &&
			d.recentDetections[i].raVersionReceivedAt.After(requiredRecency) &&
			d.recentDetections[i].systemReceivedAt.After(requiredRecency) {

			// share initial status with client, then store pointer that allows detector to update status to client
			c.RecentlySeenVersion(d.recentDetections[i].raVersion)
			c.RecentlySeenSystem(d.recentDetections[i].system)
			c.RecentlyOkMaxSendSize(d.recentDetections[i].udpMaxSendSize)
			c.RecentlyOkMaxRecvSize(d.recentDetections[i].udpMaxRecvSize)
			d.detectoredClients[i] = append(d.detectoredClients[i], c)
			detectorFound = true
		}
	}

	if (!detectorFound) {
		// tell client not to expect any detector updates; it's on its own
		c.RecentlyOkMaxSendSize(udpSizes[0])
		c.RecentlyOkMaxRecvSize(udpSizes[0])
		c.RecentlySeenVersion("")
		log.Printf("retroarch: no detector found for just-opened device %v\n", uri)
	}

	q = c
	return
}

func (d *Driver) startDetectors(addresses []*net.UDPAddr) {
	// detect retroarch in the background
	for detectorIndex, addr := range addresses {

		conn, err := d.dialUDPWithGoodLocalPort(addr)
		if err != nil {
			log.Printf("retroarch: error: detector for %v has failed to create. connection error: %v",
				addr,
				err)
			continue
		}

		d.recentDetections[detectorIndex].udpMaxSendSize = udpSizes[0]
		d.recentDetections[detectorIndex].udpMaxRecvSize = udpSizes[0]

		// start a writer
		go func(d *Driver, conn *net.UDPConn) {
			var err error

			for {
				if logDetector {
					log.Printf("retroarch: detector write to %v > VERSION", conn.RemoteAddr().(*net.UDPAddr))
				}
				_, err = conn.Write([]byte("VERSION\n"))
				if err != nil {
					log.Printf("retroarch: error: detector for %v has died due to write error %v",
						conn.RemoteAddr().(*net.UDPAddr),
						err)
					conn.Close()
					return
				}
				if logDetector {
					log.Printf("retroarch: detector write to %v > GET_STATUS", conn.RemoteAddr().(*net.UDPAddr))
				}
				_, err = conn.Write([]byte("GET_STATUS\n"))
				if err != nil {
					log.Printf("retroarch: error: detector for %v has died due to write error %v",
						conn.RemoteAddr().(*net.UDPAddr),
						err)
					conn.Close()
					return
				}
				select {
				case <-d.detectorsCloser:
					conn.Close()
					return
				case <-time.After(detectInterval):
					// back to beginning of loop; repeats the sends
				}
			}

		}(d, conn)

		// start a reader
		go func(d *Driver, conn *net.UDPConn, detectorIndex int) {

			conn.SetReadDeadline(time.Time{}) // no timeout, except if connection closes
			message := make([]byte, 65536)
			for {
				n, err := conn.Read(message)
				if err != nil {
					select {
					case <-d.detectorsCloser:
						err = nil
						return
					default:
					}
				}
				if err != nil {
					log.Printf("retroarch: error: detector for %v has died due to read error %v",
						conn.RemoteAddr().(*net.UDPAddr),
						err)
					conn.Close()
					return
				}
				message = message[:n]

				if major, minor, patch, ok := retroarch.RAParseVersion(message); ok {

					if logDetector {
						log.Printf("retroarch: detector received version response from %v < %s",
							conn.RemoteAddr().(*net.UDPAddr),
							strings.TrimSpace(string(message)))
					}
					versionString := fmt.Sprintf("%d.%d.%d", major, minor, patch)

					d.detectorsLock.RLock()
					{
						if d.recentDetections[detectorIndex].raVersion != versionString {
							for _, client := range d.detectoredClients[detectorIndex] {
								client.RecentlySeenVersion(versionString)
							}
						}
						d.recentDetections[detectorIndex].raVersion = versionString
						d.recentDetections[detectorIndex].raVersionReceivedAt = time.Now()
					}
					d.detectorsLock.RUnlock()

				} else if raStatus, systemId, _, _, ok := retroarch.RAParseGetStatus(message); ok {

					if logDetector {
						log.Printf("retroarch: detector received status response from %v < %s",
							conn.RemoteAddr().(*net.UDPAddr),
							strings.TrimSpace(string(message)))
					}

					if raStatus != "CONTENTLESS" { // CONTENTLESS means no systemId
						d.detectorsLock.RLock()
						{
							if d.recentDetections[detectorIndex].system != systemId {
								for _, client := range d.detectoredClients[detectorIndex] {
									client.RecentlySeenSystem(systemId)
								}
							}
							d.udpSizeDetectContentFound_RLockHeld(conn, detectorIndex)
							d.recentDetections[detectorIndex].system = systemId
							d.recentDetections[detectorIndex].systemReceivedAt = time.Now()
						}
						d.detectorsLock.RUnlock()
					}

				} else if d.udpSizeDetectTryParseAndHandle(detectorIndex, message) {

				} else {
					log.Printf("retroarch: detector received unknown response from %v, ignoring < %s",
						conn.RemoteAddr().(*net.UDPAddr),
						strings.TrimSpace(string(message)))
				}
				message = message[:cap(message)] // len = cap in order to read again
			}

		}(d, conn, detectorIndex)
	}
}

func (d *Driver) udpSizeDetectSend_RLockHeld(conn *net.UDPConn) {
	// send requests to RA attempting to push our send size and RA's send size to their limits.
	// lock isn't used for anything
	for _, size := range udpSizes {
		// test RA's send size
		// RA will expand our request: each device byte we asked for will be 3 text UDP bytes (e.g. " 01")
		deviceByteCountForUdpSize := (size - len("READ_CORE_RAM 0\n"))/3
		address := "0"
		// pad the response using address so the response is exactly `size` bytes
		switch (size - len("READ_CORE_RAM 0\n"))%3 {
		case 0:
			address = "0"
		case 1:
			address = "10"
		case 2:
			address = "100"
		}
		requestForALongResponse := []byte(fmt.Sprintf("READ_CORE_RAM %s %d\n", address, deviceByteCountForUdpSize))
		_, err := conn.Write(requestForALongResponse)
		if err != nil {
			log.Printf("retroarch: detect error: udpSizeDetect failed to send to %v (%v): %s",
				conn.RemoteAddr().(*net.UDPAddr),
				err,
				string(requestForALongResponse))
		} else {
			if logDetector {
				log.Printf("retroarch: detector.udpSizeDetectSend write to %v > %s",
					conn.RemoteAddr().(*net.UDPAddr),
					strings.TrimSpace(string(requestForALongResponse)))
			}
		}

		// test our send size
		if size > maxUdpPacketToTrySending {
			continue
		}
		// bury our actual request, for which we expect a response, at the end of a long series of newlines,
		// which RA will seek past
		// also we sneakily pretend to RA that we care what's at address `size`. we don't, it could be nothing.
		// but this causes RA to echo `size` back to us as the address, from which we'll know that RA heard the
		// request of size `size`
		buriedRequest := []byte(fmt.Sprintf("READ_CORE_MEMORY %x 1\n", size))
		sendbuf := make([]byte, size-len(buriedRequest), size)
		for i, _ := range sendbuf {
			sendbuf[i] = byte('\n')
		}
		sendbuf = append(sendbuf, buriedRequest...)
		_, err = conn.Write(sendbuf)
		if err != nil {
			// ignore error and don't log as sent
		} else {
			if logDetector {
				log.Printf("retroarch: detector.udpSizeDetectSend write to %v (newlines padding to size %d) %s",
					conn.RemoteAddr().(*net.UDPAddr),
					size,
					strings.TrimSpace(string(buriedRequest)))
			}
		}
	}
}

func (d *Driver) udpSizeDetectContentFound_RLockHeld(conn *net.UDPConn, detectorIndex int) {
	// called every time RA content is detected, which is often enough for us to use as a time interval for
	// udp size detect start & timeout (a much more infrequent process).
	if !d.recentDetections[detectorIndex].udpSizeDetectStateActive &&
		d.recentDetections[detectorIndex].udpSizeDetectStateChangedAt.Before(
			time.Now().Add(-time.Minute*5)) {

		// start
		// requires that there be content so that we can use READ_CORE_RAM 0 to test max read size
		// (doesn't matter what core, just that a core is loaded)
		d.recentDetections[detectorIndex].udpSendSizeOk = map[int]struct{}{}
		d.recentDetections[detectorIndex].udpRecvSizeOk = map[int]struct{}{}
		d.recentDetections[detectorIndex].udpSizeDetectStateActive = true
		d.recentDetections[detectorIndex].udpSizeDetectStateChangedAt = time.Now()
		d.udpSizeDetectSend_RLockHeld(conn)

	} else if d.recentDetections[detectorIndex].udpSizeDetectStateActive &&
		d.recentDetections[detectorIndex].udpSizeDetectStateChangedAt.Before(
			time.Now().Add(-time.Second*15)) {

		// timeout
		d.recentDetections[detectorIndex].udpSizeDetectStateActive = false
		d.recentDetections[detectorIndex].udpSizeDetectStateChangedAt = time.Now()
		d.udpSizeDetectUpdate_RLockHeld(detectorIndex)
	}
}

func (d *Driver) udpSizeDetectUpdate_RLockHeld(detectorIndex int) {
	testsBegun := len(udpSizes)
	testsCompleted := 0
	maxSendSize := udpSizes[0]
	maxRecvSize := udpSizes[0]
	for _, size := range udpSizes {
		if size <= maxUdpPacketToTrySending {
			testsBegun++
		}
	}
	for size, _ := range d.recentDetections[detectorIndex].udpSendSizeOk {
		testsCompleted++
		if size > maxSendSize {
			maxSendSize = size
		}
	}
	for size, _ := range d.recentDetections[detectorIndex].udpRecvSizeOk {
		testsCompleted++
		if size > maxRecvSize {
			maxRecvSize = size
		}
	}
	if !d.recentDetections[detectorIndex].udpSizeDetectStateActive || testsBegun == testsCompleted {
		// udp size detect is completed (incl. timed out, signified by !udpSizeDetectStateActive)
		// inform any RA clients if the size changed
		if maxSendSize != d.recentDetections[detectorIndex].udpMaxSendSize {
			d.recentDetections[detectorIndex].udpMaxSendSize = maxSendSize
			for _, client := range d.detectoredClients[detectorIndex] {
				client.RecentlyOkMaxSendSize(d.recentDetections[detectorIndex].udpMaxSendSize)
			}
		}
		if maxRecvSize != d.recentDetections[detectorIndex].udpMaxRecvSize {
			d.recentDetections[detectorIndex].udpMaxRecvSize = maxRecvSize
			for _, client := range d.detectoredClients[detectorIndex] {
				client.RecentlyOkMaxRecvSize(d.recentDetections[detectorIndex].udpMaxRecvSize)
			}
		}
		if d.recentDetections[detectorIndex].udpSizeDetectStateActive {
			d.recentDetections[detectorIndex].udpSizeDetectStateActive = false
			d.recentDetections[detectorIndex].udpSizeDetectStateChangedAt = time.Now()
		}
		if logDetector {
			log.Printf("retroarch: detect(%v): max send UDP=%d, max recv UDP=%d",
				d.detectorAddrs[detectorIndex],
				maxSendSize,
				maxRecvSize)
		}
	}
}

func (d *Driver) udpSizeDetectTryParseAndHandle(detectorIndex int, message []byte) (ok bool) {
	d.detectorsLock.RLock()
	defer d.detectorsLock.RUnlock()

	if bytes.HasPrefix(message, []byte("READ_CORE_RAM")) {
		// this came back from a large read test
		ok = true
		d.recentDetections[detectorIndex].udpRecvSizeOk[len(message)] = struct{}{}
		if logDetector {
			log.Printf("retroarch: detector.udpSizeDetect for %v got back a large read UDP=%d < %s...",
				d.detectorAddrs[detectorIndex],
				len(message),
				string(message)[:21])
		}
		d.udpSizeDetectUpdate_RLockHeld(detectorIndex)
	} else if bytes.HasPrefix(message, []byte("READ_CORE_MEMORY")) {
		// this came back from a large write(i.e. large UDP send, not a memory write) test
		ok = true
		splits := bytes.SplitN(message, []byte(" "), 3)
		if len(splits) < 3 {
			log.Printf("retroarch: detector for %v received unexpected response (not enough spaces), ignoring < %s",
				d.detectorAddrs[detectorIndex],
				strings.TrimSpace(string(message)))
			return
		}
		var lengthOfOriginalMessage int
		_, err := fmt.Sscanf(string(splits[1]), "%x", &lengthOfOriginalMessage)
		if err != nil {
			log.Printf("retroarch: detector for %v recevied non-hex, ignoring < %s",
				d.detectorAddrs[detectorIndex],
				strings.TrimSpace(string(message)))
			return
		}
		d.recentDetections[detectorIndex].udpSendSizeOk[lengthOfOriginalMessage] = struct{}{}
		if logDetector {
			log.Printf("retroarch: detector.udpSizeDetect for %v got back a response to large write of UDP=%d < %s",
				d.detectorAddrs[detectorIndex],
				lengthOfOriginalMessage,
				strings.TrimSpace(string(message)))
		}
		d.udpSizeDetectUpdate_RLockHeld(detectorIndex)
	} else {
		ok = false
	}
	return
}

func (d *Driver) dialUDPWithGoodLocalPort(raddr *net.UDPAddr) (conn *net.UDPConn, err error) {
	numPortAllocRetries := 30

	for numPortAllocRetries > 0 {

		conn, err = net.DialUDP("udp", nil, raddr)
		if err != nil {
			if logDetector {
				log.Printf("retroarch: connect local %s -> remote %s: %v\n", conn.LocalAddr().String(), conn.RemoteAddr().String(), err)
			}
			return
		}

		// return if we allocated a good port
		if _, ok := d.localUdpPortsToAvoid[conn.LocalAddr().(*net.UDPAddr).Port]; !ok {
			return // success
		}

		// retry. assumes multiple Connect() attempts tend to use different local ports
		if logDetector {
			log.Printf("retroarch: discarding just connected local udp %v -> remote udp %v to avoid future conflict. reconnecting", conn.LocalAddr(), conn.RemoteAddr())
		}
		conn.Close()
		conn = nil
		numPortAllocRetries--
	}
	err = fmt.Errorf("Could not allocate a port outside localUdpPortsToAvoid")
	return
}

func (d *Driver) Detect() (devs []devices.DeviceDescriptor, err error) {
	// for each detector: if we got both version + status responses in the last 5 seconds, it's a valid device
	requiredRecency := time.Now().Add(-time.Second * 5)
	d.detectorsLock.Lock()
	defer d.detectorsLock.Unlock()

	for i, recent := range d.recentDetections {

		if recent.raVersionReceivedAt.After(requiredRecency) &&
			recent.systemReceivedAt.After(requiredRecency) {

			descriptor := devices.DeviceDescriptor{
				Uri:                 url.URL{Scheme: driverName, Host: d.detectorAddrs[i].String()},
				DisplayName:         fmt.Sprintf("RetroArch v%s (%s)", recent.raVersion, d.detectorAddrs[i]),
				Kind:                d.Kind(),
				Capabilities:        driverCapabilities[:],
				DefaultAddressSpace: sni.AddressSpace_SnesABus,
				System:              "snes",
			}

			devs = append(devs, descriptor)
		}
	}

	err = nil
	return
}

func (d *Driver) DeviceKey(uri *url.URL) string {
	return uri.Host
}

func (d *Driver) Device(uri *url.URL) devices.AutoCloseableDevice {
	return devices.NewAutoCloseableDevice(d.container, uri, d.DeviceKey(uri))
}

func (d *Driver) DisconnectAll() {
	for _, deviceKey := range d.container.AllDeviceKeys() {
		device, ok := d.container.GetDevice(deviceKey)
		if ok {
			log.Printf("%s: disconnecting device '%s'\n", driverName, deviceKey)
			_ = device.Close()
			d.container.DeleteDevice(deviceKey)
		}
	}
}

func DriverInit() {
	if util.IsTruthy(env.GetOrDefault("SNI_RETROARCH_DISABLE", "0")) {
		log.Printf("disabling retroarch snes driver\n")
		return
	}

	// comma-delimited list of host:port pairs:
	hostsStr := env.GetOrSupply("SNI_RETROARCH_HOSTS", func() string {
		// default network_cmd_port for RA is UDP 55355. we want to support connecting to multiple
		// instances so let's auto-detect RA instances listening on UDP ports in the range
		// [55355..55362]. realistically we probably won't be running any more than a few instances on
		// the same machine at one time. i picked 8 since i currently have an 8-core CPU :)
		var sb strings.Builder
		const count = 1
		for i := 0; i < count; i++ {
			sb.WriteString(fmt.Sprintf("localhost:%d", 55355+i))
			if i < count-1 {
				sb.WriteByte(',')
			}
		}
		return sb.String()
	})

	// split the hostsStr list by commas:
	hosts := strings.Split(hostsStr, ",")

	// resolve the addresses:
	addresses := make([]*net.UDPAddr, 0, len(hosts))
	for _, host := range hosts {
		addr, err := net.ResolveUDPAddr("udp", host)
		if err != nil {
			log.Printf("retroarch: resolve('%s'): %v\n", host, err)
			// drop the address if it doesn't resolve:
			// TODO: consider retrying the resolve later? maybe not worth worrying about.
			continue
		}

		addresses = append(addresses, addr)
	}

	if util.IsTruthy(env.GetOrDefault("SNI_RETROARCH_DETECT_LOG", "0")) {
		logDetector = true
		log.Printf("enabling retroarch detector logging")
	}

	// start connecting to addresses and register the driver:
	driver = NewDriver(addresses)
	devices.Register(driverName, driver)
}
