package measurements

import (
	"log"
	"os"
	"time"
)

/*
CONSTANTS for the Bitswap performance measurements
*/
const (
	LOGFILE           = "bitswap.log"
	PACKETSLOGS       = "packets.log"
	DISCOVERY_TIMEOUT = 15 * time.Second
	DHT_TIMEOUT       = 35 * time.Second

	NO_PROVS     = "NO_PROVS"
	FETCH_FAILED = "FETCH_FAILED"
)

func LogStringToFile(str, filename string) {
	// If the file doesn't exist, create it, otherwise append to the file
	f, err := os.OpenFile(filename,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := f.WriteString(str + "\n"); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
