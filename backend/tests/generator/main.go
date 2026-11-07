// A standalone test-data tool: no database, credentials, networking or runtime API imports.
package main

import (
	"flag"
	"fmt"
	"github.com/customer-fabric/customer-fabric/backend/tests/enterprise"
	"os"
)

func main() {
	config := flag.String("config", "", "JSON configuration (defaults if omitted)")
	preset := flag.String("preset", "medium", "small=100, medium=10000, large=100000")
	count := flag.Int("population", 0, "override population")
	out := flag.String("out", "", "new output directory; never overwritten")
	validate := flag.String("validate", "", "validate an existing dataset without writes")
	replay := flag.String("replay", "", "stream an existing delivery.ndjson to stdout; no API writes")
	curated := flag.String("curated", "", "write standalone curated samples to a new directory")
	shuffled := flag.Bool("shuffled", false, "shuffle replay deterministically (seed 42)")
	flag.Parse()
	die := func(e error) {
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}
	if *curated != "" {
		die(enterprise.WriteCurated(*curated))
		return
	}
	if *validate != "" {
		die(enterprise.Validate(*validate))
		fmt.Println("PASS: fixture consistency")
		return
	}
	if *replay != "" {
		if *shuffled {
			die(enterprise.ReplayShuffled(*replay, os.Stdout, 42))
		} else {
			die(enterprise.Replay(*replay, os.Stdout))
		}
		return
	}
	c := enterprise.DefaultConfig()
	if *config != "" {
		var e error
		c, e = enterprise.LoadConfig(*config)
		die(e)
	}
	// Explicit presets override config only when supplied, leaving config population meaningful.
	presetSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "preset" {
			presetSet = true
		}
	})
	if presetSet || *config == "" {
		switch *preset {
		case "small":
			c.Population = 100
		case "medium":
			c.Population = 10000
		case "large":
			c.Population = 100000
		default:
			die(fmt.Errorf("unknown preset"))
		}
	}
	if *count != 0 {
		c.Population = *count
	}
	if *out == "" {
		die(fmt.Errorf("-out is required"))
	}
	s, e := enterprise.Generate(c, *out)
	die(e)
	die(enterprise.Validate(*out))
	fmt.Printf("PASS: %d synthetic individuals, %d deliveries; %s\n", s.People, s.Events, *out)
}
