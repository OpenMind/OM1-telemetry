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
//	zenohdup -d 30s scan odom camera/realsense2_camera_node/depth/image_rect_raw
package main

import (
	"crypto/md5"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/eclipse-zenoh/zenoh-go/zenoh"
)

func main() {
	endpoint := flag.String("e", "tcp/127.0.0.1:7447", "Zenoh endpoint to connect to")
	duration := flag.Duration("d", 12*time.Second, "how long to sample")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: zenohdup [-e endpoint] [-d duration] <key-expression>...\n\n")
		fmt.Fprintf(os.Stderr, "Reports the delivery multiple for a Zenoh key: 1.00x is healthy,\n")
		fmt.Fprintf(os.Stderr, "2.00x means every message is arriving twice.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

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

	fmt.Printf("%-52s %10s %8s %8s  %s\n", "TOPIC", "RECEIVED", "UNIQUE", "RATIO", "VERDICT")
	fmt.Println(strings.Repeat("-", 92))

	duplicated := 0
	for _, key := range flag.Args() {
		if sampleKey(session, key, *duration) {
			duplicated++
		}
	}

	fmt.Println()
	if duplicated == 0 {
		fmt.Printf("All %d topic(s) healthy: every message delivered exactly once.\n", flag.NArg())
		return
	}
	fmt.Printf("%d of %d topic(s) duplicated.\n\n", duplicated, flag.NArg())
	fmt.Println("Copies sharing one source id are the zenoh-bridge-ros2dds stale-route bug")
	fmt.Println("(github.com/eclipse-zenoh/zenoh-plugin-ros2dds issue #570): the bridge keeps")
	fmt.Println("forwarding for ROS publishers that no longer exist, one extra copy for every")
	fmt.Println("om1_sensor restart it has survived. Clear it with:")
	fmt.Println()
	fmt.Println("    docker restart zenoh_bridge")
	fmt.Println()
	fmt.Println("and restart with OM1-ros2-sdk/zenoh/restart_sensors.sh in future, which")
	fmt.Println("restarts the bridge last so it never accumulates them.")
	os.Exit(1)
}

// sampleKey subscribes for the given duration and reports one table row.
// Returns true if the topic is being delivered more than once.
func sampleKey(session zenoh.Session, key string, duration time.Duration) bool {
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		fmt.Printf("%-52s %s\n", key, "bad key expression")
		return false
	}
	handler := zenoh.NewFifoChannel[zenoh.Sample](8192)
	sub, err := session.DeclareSubscriber(ke, handler, nil)
	if err != nil {
		fmt.Printf("%-52s %s\n", key, "cannot subscribe")
		return false
	}
	defer sub.Drop()

	// Payload digest -> how many copies of it arrived. Comparing content rather
	// than counting messages is what separates duplication from a topic that is
	// simply fast.
	copies := map[[16]byte]int{}
	bySource := map[string]int{}
	total := 0

	deadline := time.After(duration)
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
		fmt.Printf("%-52s %10s %8s %8s  %s\n", key, "-", "-", "-", "NOT PUBLISHED")
		return false
	}

	unique := len(copies)
	ratio := float64(total) / float64(unique)
	verdict := "OK"
	if ratio >= 1.5 {
		verdict = "DUPLICATED"
	}

	sources := make([]string, 0, len(bySource))
	for src := range bySource {
		sources = append(sources, src)
	}
	sort.Strings(sources)

	detail := fmt.Sprintf("%d source", len(sources))
	if len(sources) != 1 {
		detail += "s"
	}
	fmt.Printf("%-52s %10d %8d %7.2fx  %-11s %s\n",
		key, total, unique, ratio, verdict, detail)

	return verdict == "DUPLICATED"
}
