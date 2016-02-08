package procspy

import (
	"bytes"
	"fmt"
	"math"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/weaveworks/scope/probe/process"
)

const (
	initialRateLimitPeriod = 50 * time.Millisecond  // Read 20 * fdBlockSize file descriptors (/proc/PID/fd/*) per namespace per second
	maxRateLimitPeriod     = 250 * time.Millisecond // Read at least 4 * fdBlockSize file descriptors per namespace per second
	fdBlockSize            = uint64(300)            // Maximum number of /proc/PID/fd/* files to stat per rate-limit period
	// (as a rule of thumb going through each block should be more expensive than reading /proc/PID/tcp{,6})
	targetWalkTime = 10 * time.Second // Aim at walking all files in 10 seconds
)

type backgroundReader struct {
	walker       process.Walker
	mtx          sync.Mutex
	running      bool
	pleaseStop   bool
	walkingBuf   *bytes.Buffer
	readyBuf     *bytes.Buffer
	readySockets map[uint64]*Proc
}

func newBackgroundReader(walker process.Walker) *backgroundReader {
	br := &backgroundReader{
		walker:     walker,
		walkingBuf: bytes.NewBuffer(make([]byte, 0, 5000)),
		readyBuf:   bytes.NewBuffer(make([]byte, 0, 5000)),
	}
	return br
}

// starts a rate-limited background goroutine to read the expensive files from
// proc.
func (br *backgroundReader) start() error {
	br.mtx.Lock()
	defer br.mtx.Unlock()
	if br.running {
		return fmt.Errorf("background reader already running")
	}
	br.running = true
	go br.loop()
	return nil
}

func (br *backgroundReader) stop() error {
	br.mtx.Lock()
	defer br.mtx.Unlock()
	if !br.running {
		return fmt.Errorf("background reader already not running")
	}
	br.pleaseStop = true
	return nil
}

func (br *backgroundReader) loop() {
	const (
		maxRateLimitPeriodF = float64(maxRateLimitPeriod)
		targetWalkTimeF     = float64(targetWalkTime)
	)

	rateLimitPeriod := initialRateLimitPeriod
	ticker := time.NewTicker(rateLimitPeriod)
	for {
		start := time.Now()
		sockets, err := walkProcPid(br.walkingBuf, br.walker, ticker.C, fdBlockSize)
		if err != nil {
			log.Errorf("background /proc reader: error walking /proc: %s", err)
			continue
		}

		br.mtx.Lock()

		// Should we stop?
		if br.pleaseStop {
			br.pleaseStop = false
			br.running = false
			ticker.Stop()
			br.mtx.Unlock()
			return
		}

		// Swap buffers
		br.readyBuf, br.walkingBuf = br.walkingBuf, br.readyBuf
		br.readySockets = sockets

		br.mtx.Unlock()

		walkTime := time.Now().Sub(start)
		walkTimeF := float64(walkTime)

		log.Debugf("background /proc reader: full pass took %s", walkTime)
		if walkTimeF/targetWalkTimeF > 1.5 {
			log.Warnf(
				"background /proc reader: full pass took %s: 50%% more than expected (%s)",
				walkTime,
				targetWalkTime,
			)
		}

		// Adjust rate limit to more-accurately meet the target walk time in next iteration
		scaledRateLimitPeriod := targetWalkTimeF / walkTimeF * float64(rateLimitPeriod)
		rateLimitPeriod = time.Duration(math.Min(scaledRateLimitPeriod, maxRateLimitPeriodF))
		log.Debugf("background /proc reader: new rate limit %s", rateLimitPeriod)

		ticker.Stop()
		ticker = time.NewTicker(rateLimitPeriod)

		br.walkingBuf.Reset()

		// Sleep during spare time
		time.Sleep(targetWalkTime - walkTime)
	}
}

func (br *backgroundReader) getWalkedProcPid(buf *bytes.Buffer) map[uint64]*Proc {
	br.mtx.Lock()
	defer br.mtx.Unlock()

	reader := bytes.NewReader(br.readyBuf.Bytes())
	buf.ReadFrom(reader)

	return br.readySockets
}
