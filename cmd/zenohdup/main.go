// Command zenohdup reports whether a Zenoh topic is being delivered more than
// once, and by whom.
//
// It exists because a duplicated topic is almost invisible from the recordings
// alone: the files are larger and the rate is a clean multiple of nominal, but
// every frame decodes and no sequence number is missing. On a G1 this was
// happening to /scan and both RealSense topics at 2x and 3x, with byte-
// identical copies arriving a tenth of a millisecond apart.
//
// The cause was zenoh-bridge-ros2dds: it adds a forwarding route when it
// discovers a ROS publisher and does not remove it when that publisher goes
// away, so each om1_sensor restart the bridge survives leaves another stale
// route forwarding the same message. Restarting the bridge clears them; see
// OM1-ros2-sdk's zenoh/restart_sensors.sh.
//
// Reporting the source id matters: copies sharing one id mean a single
// publisher sending repeatedly (the bug above), while copies from different
// ids would mean genuinely separate routes onto the bus -- a different problem
// with a different fix.
//
//	zenohdup scan
//	zenohdup -d 30s camera/realsense2_camera_node/depth/image_rect_raw
package main

import (
	"crypto/md5"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"
)

func main() {
	endpoint := flag.String("e", "tcp/127.0.0.1:7447", "Zenoh endpoint to connect to")
	duration := flag.Duration("d", 12*time.Second, "how long to sample")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: zenohdup [-e endpoint] [-d duration] <key-expression>\n\n")
		fmt.Fprintf(os.Stderr, "Reports the delivery multiple for a Zenoh key: 1.00x is healthy,\n")
		fmt.Fprintf(os.Stderr, "2.00x means every message is arriving twice.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	key := flag.Arg(0)

	cfg := zenoh.NewConfigDefault()
	if err := cfg.InsertJson5(zenoh.ConfigModeKey, `"client"`); err != nil {
		fmt.Fprintln(os.Stderr, "set mode:", err)
		os.Exit(1)
	}
	if err := cfg.InsertJson5(zenoh.ConfigConnectKey, `["`+*endpoint+`"]`); err != nil {
		fmt.Fprintln(os.Stderr, "set endpoint:", err)
		os.Exit(1)
	}

	session, err := zenoh.Open(cfg, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open session:", err)
		os.Exit(1)
	}
	defer session.Drop()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad key expression:", err)
		os.Exit(1)
	}
	handler := zenoh.NewFifoChannel[zenoh.Sample](8192)
	sub, err := session.DeclareSubscriber(ke, handler, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "declare subscriber:", err)
		os.Exit(1)
	}
	defer sub.Drop()

	// Payload digest -> how many copies of it arrived. Comparing content rather
	// than counting messages is what separates duplication from a topic that is
	// simply fast.
	copies := map[[16]byte]int{}
	bySource := map[string]int{}
	total := 0

	fmt.Fprintf(os.Stderr, "sampling %s for %s...\n", key, *duration)
	deadline := time.After(*duration)
	rx := sub.Handler()
loop:
	for {
		select {
		case <-deadline:
			break loop
		case sample, ok := <-rx:
			if !ok {
				break loop
			}
			total++
			copies[md5.Sum(sample.Payload().Bytes())]++
			source := "(no timestamp)"
			if ts := sample.TimeStamp(); ts.IsSome() {
				source = fmt.Sprintf("%v", ts.Unwrap().Id())
			}
			bySource[source]++
		}
	}

	if total == 0 {
		fmt.Printf("%s: nothing received in %s -- topic not published, or the key is wrong\n",
			key, *duration)
		os.Exit(1)
	}

	unique := len(copies)
	multiple := float64(total) / float64(unique)
	verdict := "OK"
	if multiple >= 1.5 {
		verdict = "DUPLICATED"
	}

	fmt.Printf("%s\n", key)
	fmt.Printf("  %-10s %d received, %d unique  -> %.2fx  [%s]\n",
		"delivery:", total, unique, multiple, verdict)

	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	fmt.Printf("  %-10s\n", "sources:")
	for _, s := range sources {
		fmt.Printf("    %s  %d\n", s, bySource[s])
	}

	if verdict == "DUPLICATED" {
		if len(sources) == 1 {
			fmt.Printf("\n  One source sending each message %.0f times. This is the\n", multiple)
			fmt.Printf("  zenoh-bridge-ros2dds stale-route bug: restart the bridge\n")
			fmt.Printf("  (OM1-ros2-sdk zenoh/restart_sensors.sh) to clear it.\n")
		} else {
			fmt.Printf("\n  Copies from %d different sources: separate routes onto the bus,\n", len(sources))
			fmt.Printf("  not the stale-route bug. Check which bridges forward this key.\n")
		}
		os.Exit(1)
	}
}
