package retroarch

import (
	"fmt"
	"log"
	"net"
	"net/url"
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
	detectorsLock    sync.RWMutex  // inverted meaning: RLock() to write own entry, Lock() to read all entries
	recentDetections []recentDetection
	detectorAddrs    []*net.UDPAddr
}

type recentDetection struct {
	raVersion           string
	raVersionReceivedAt time.Time
	system              string
	systemReceivedAt    time.Time
}

func NewDriver(addresses []*net.UDPAddr) *Driver {
	d := &Driver{
		localUdpPortsToAvoid: make(map[int]struct{}),
		detectorsCloser:      make(chan struct{}),
		recentDetections:     make([]recentDetection, len(addresses)),
		detectorAddrs:        addresses,
	}

	// retroarch addresses are often on the local machine, so do not bind on any of their port numbers
	for _, addr := range addresses {
		d.localUdpPortsToAvoid[addr.Port] = struct{}{}
	}

	d.container = devices.NewDeviceDriverContainer(d.openDevice)

	// detect retroarch in the background
	for detectorIndex, addr := range addresses {

		conn, err := d.dialUDPWithGoodLocalPort(addr)
		if err != nil {
			log.Printf("retroarch: error: detector for %v has failed to create. connection error: %v",
				addr,
				err)
			continue
		}

		// start writer
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
				time.Sleep(detectInterval)
				select {
				case <-d.detectorsCloser:
					conn.Close()
					return
				default:
				}
			}

		}(d, conn)

		// start reader
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

					d.detectorsLock.RLock()
					{
						d.recentDetections[detectorIndex].raVersion = fmt.Sprintf("%d.%d.%d", major, minor, patch)
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
							d.recentDetections[detectorIndex].system = systemId
							d.recentDetections[detectorIndex].systemReceivedAt = time.Now()
						}
						d.detectorsLock.RUnlock()
					}

				} else {
					log.Printf("retroarch: detector received unknown response from %v, ignoring < %s",
						conn.RemoteAddr().(*net.UDPAddr),
						strings.TrimSpace(string(message)))
				}
				message = message[:cap(message)] // len = cap in order to read again
			}

		}(d, conn, detectorIndex)
	}

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

	_, err = c.DetermineVersionAndSystemAndApi(logDetector)
	if err != nil {
		_ = c.Close()
		return
	}
	c.LogRCR()

	q = c
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
