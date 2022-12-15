package retroarch

// TODO: this should go in its own folder when multiple retroarch systems are implemented. (or all systems and this file go in the same folder)

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

func RAParseGetStatus(message []byte) (raStatus, systemId, romFileName string, romCRC32 uint32, ok bool) {
	// parse the response:
	_, err := fmt.Fscanf(bytes.NewReader(message), "GET_STATUS %s ", &raStatus)
	if err != nil {
		return
	}

	// get the remainder
	var spaceSplit []string
	if raStatus != "CONTENTLESS" {
		spaceSplit = strings.SplitN(string(message), " ", 3)
	}
	if raStatus != "CONTENTLESS" && len(spaceSplit) == 3 {
		allArgsString := strings.TrimSpace(spaceSplit[2])
		// split the remainder by commas (note data will be wrong if rom name contains a comma):
		argsArr := strings.Split(allArgsString, ",")
		if len(argsArr) >= 1 {
			systemId = argsArr[0]
			if systemId != "super_nes" {
				// unknown system. but some RA versions around 1.9.2 put the core name as their system_id.
				// check for at least bsnes and snes9x
				if strings.Contains(strings.ToLower(systemId), "snes") {
					systemId = "super_nes"
				}
			}
		}
		if len(argsArr) >= 2 {
			romFileName = argsArr[1]
		}
		if len(argsArr) >= 3 {
			// e.g. "crc32=dae58be6"
			crc32 := argsArr[2]
			if strings.HasPrefix(crc32, "crc32=") {
				crc32 = crc32[len("crc32="):]

				var crc32_u64 uint64
				crc32_u64, err = strconv.ParseUint(crc32, 16, 32)
				if err == nil {
					romCRC32 = uint32(crc32_u64)
				}
			}
		}
	}

	ok = true
	return
}

func RAParseVersion(message []byte) (major int, minor int, patch int, ok bool) {
	n, err := fmt.Sscanf(strings.TrimSpace(string(message)), "%d.%d.%d", &major, &minor, &patch)
	if err != nil || n != 3 {
		ok = false
		return
	}
	if strings.TrimSpace(string(message)) != fmt.Sprintf("%d.%d.%d", major, minor, patch) {
		// extra data?
		ok = false
		return
	}
	ok = true
	return
}
