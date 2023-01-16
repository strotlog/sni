package retroarch

import (
	"fmt"
	"github.com/alttpo/snes/timing"
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

type Driver struct {
	container devices.DeviceContainer

	detectors []*retroarch.RAClient
}

func NewDriver(addresses []*net.UDPAddr) *Driver {
	d := &Driver{
		detectors: make([]*retroarch.RAClient, len(addresses)),
	}
	d.container = devices.NewDeviceDriverContainer(d.openDevice)

	for i, addr := range addresses {
		c := retroarch.NewRAClient(addr, fmt.Sprintf("retroarch[%d]", i), timing.Frame*4)
		d.detectors[i] = c
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

	var c *retroarch.RAClient
	c = retroarch.NewRAClient(addr, addr.String(), time.Second*5)
	err = c.Connect(addr)
	if err != nil {
		return
	}

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

func (d *Driver) Detect() (devs []devices.DeviceDescriptor, err error) {
	devicesLock := sync.Mutex{}
	devs = make([]devices.DeviceDescriptor, 0, len(d.detectors))

	wg := sync.WaitGroup{}
	wg.Add(len(d.detectors))
	for i, de := range d.detectors {
		// run detectors in parallel:
		go func(i int, detector *retroarch.RAClient) {
			defer util.Recover()
			defer wg.Done()

			detector.MuteLog(true)

			// reopen detector if necessary:
			if detector.IsClosed() {
				detector.Close()
				// refresh detector:
				c := retroarch.NewRAClient(detector.GetRemoteAddr(), fmt.Sprintf("retroarch[%d]", i), timing.Frame*4)
				d.detectors[i] = c
				c.MuteLog(true)
				detector = c
			}

			// reconnect detector if necessary:
			if !detector.IsConnected() {
				// "connect" to this UDP endpoint:
				err = detector.Connect(detector.GetRemoteAddr())
				if err != nil {
					if logDetector {
						log.Printf("retroarch: detect: detector[%d]: connect: %v\n", i, err)
					}
					return
				}
				if detector.DetectLoopback(d.detectors) {
					detector.Close()
					if logDetector {
						log.Printf("retroarch: detect: detector[%d]: loopback connection detected; breaking\n", i)
					}
					return
				}
			}

			// we need to check if the retroarch device is listening and running a snes:
			var systemId string
			systemId, err = detector.DetermineVersionAndSystemAndApi(logDetector)
			if err != nil {
				if logDetector {
					log.Printf("retroarch: detect: detector[%d]: %s\n", i, err)
				}
				detector.Close()
				return
			}
			if !detector.HasVersion() {
				return
			}
			if systemId == "" {
				if logDetector {
					log.Printf("retroarch: no system loaded\n")
				}
				return
			}
			if systemId != "super_nes" {
				if logDetector {
					log.Printf("retroarch: running unrecognized system_id '%s'\n", systemId)
				}
				return
			}

			descriptor := devices.DeviceDescriptor{
				Uri:                 url.URL{Scheme: driverName, Host: detector.GetRemoteAddr().String()},
				DisplayName:         fmt.Sprintf("RetroArch v%s (%s)", detector.Version(), detector.GetRemoteAddr()),
				Kind:                d.Kind(),
				Capabilities:        driverCapabilities[:],
				DefaultAddressSpace: sni.AddressSpace_SnesABus,
				System:              "snes",
			}

			devicesLock.Lock()
			devs = append(devs, descriptor)
			devicesLock.Unlock()
		}(i, de)
	}
	wg.Wait()

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

	// register the driver:
	driver = NewDriver(addresses)
	devices.Register(driverName, driver)
}
