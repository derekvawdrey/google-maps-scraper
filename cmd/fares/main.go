// Command gmaps-fares reads "origin -> destination" pairs and uses the
// google-maps-scraper browser engine to extract the transit fare Google Maps
// shows for each route.
//
// Usage:
//
//	gmaps-fares -input routes.txt -results fares.csv
//	echo "Tokyo Station -> Shibuya Station" | gmaps-fares
//
// Each input line is "<origin> -> <destination>". Blank lines and lines
// starting with '#' are ignored.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/google-maps-scraper/gmaps"
	"github.com/gosom/scrapemate"
	"github.com/gosom/scrapemate/adapters/writers/csvwriter"
	"github.com/gosom/scrapemate/adapters/writers/jsonwriter"
	"github.com/gosom/scrapemate/scrapemateapp"
)

func main() {
	var (
		inputFile   = flag.String("input", "stdin", "input file with 'origin -> destination' lines, or 'stdin'")
		resultsFile = flag.String("results", "stdout", "output file, or 'stdout'")
		asJSON      = flag.Bool("json", false, "write JSON instead of CSV")
		concurrency = flag.Int("c", 1, "number of concurrent browser pages")
		langCode    = flag.String("lang", "en", "Google Maps language code (hl), e.g. en or ja")
		mode        = flag.String("mode", "transit", "travel mode: transit, driving, walking, bicycling")
		preferBus   = flag.Bool("prefer-bus", false, "best-effort: bias transit results toward buses")
		timeout     = flag.Duration("timeout", 0, "overall safety timeout (0 = auto from route count)")
	)

	flag.Parse()

	if err := run(*inputFile, *resultsFile, *asJSON, *concurrency, *langCode, *mode, *preferBus, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(inputFile, resultsFile string, asJSON bool, concurrency int, langCode, mode string, preferBus bool, timeout time.Duration) error {
	in, closeIn, err := openInput(inputFile)
	if err != nil {
		return err
	}
	defer closeIn()

	monitor := exiter.New()

	jobs, err := createJobs(in, langCode, mode, preferBus, monitor)
	if err != nil {
		return err
	}

	if len(jobs) == 0 {
		return fmt.Errorf("no 'origin -> destination' pairs found in input")
	}

	monitor.SetSeedCount(len(jobs))

	out, closeOut, err := openOutput(resultsFile)
	if err != nil {
		return err
	}
	defer closeOut()

	var writer scrapemate.ResultWriter
	if asJSON {
		writer = jsonwriter.NewJSONWriter(out)
	} else {
		writer = csvwriter.NewCsvWriter(csv.NewWriter(out))
	}

	app, err := scrapemateapp.NewScrapeMateApp(mustConfig(writer, concurrency))
	if err != nil {
		return err
	}

	defer app.Close()

	// The exiter cancels the context the moment every route has been processed.
	// A generous overall timeout is the only backstop against a hung browser.
	if timeout <= 0 {
		timeout = time.Duration(len(jobs))*90*time.Second + 2*time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	monitor.SetCancelFunc(cancel)

	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sig
		cancel()
	}()

	go monitor.Run(ctx)

	fmt.Fprintf(os.Stderr, "looking up %d route(s) (mode=%s)...\n", len(jobs), mode)

	if err := app.Start(ctx, jobs...); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}

func mustConfig(writer scrapemate.ResultWriter, concurrency int) *scrapemateapp.Config {
	cfg, err := scrapemateapp.NewConfig(
		[]scrapemate.ResultWriter{writer},
		scrapemateapp.WithConcurrency(concurrency),
		scrapemateapp.WithJS(scrapemateapp.DisableImages()),
	)
	if err != nil {
		panic(err)
	}

	return cfg
}

func createJobs(r io.Reader, langCode, mode string, preferBus bool, monitor exiter.Exiter) ([]scrapemate.IJob, error) {
	var jobs []scrapemate.IJob

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		origin, destination, ok := splitPair(line)
		if !ok {
			return nil, fmt.Errorf("invalid line %q: expected '<origin> -> <destination>'", line)
		}

		jobs = append(jobs, gmaps.NewDirectionsJob(
			"", origin, destination, mode, langCode, preferBus,
			gmaps.WithDirectionsJobExitMonitor(monitor),
		))
	}

	return jobs, scanner.Err()
}

// splitPair splits "<origin> -> <destination>". It also accepts a tab or the
// "#!#" delimiter used elsewhere in the project.
func splitPair(line string) (origin, destination string, ok bool) {
	for _, sep := range []string{"->", "\t", "#!#"} {
		if before, after, found := strings.Cut(line, sep); found {
			origin = strings.TrimSpace(before)
			destination = strings.TrimSpace(after)

			return origin, destination, origin != "" && destination != ""
		}
	}

	return "", "", false
}

func openInput(name string) (io.Reader, func(), error) {
	if name == "stdin" || name == "" {
		return os.Stdin, func() {}, nil
	}

	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}

	return f, func() { _ = f.Close() }, nil
}

func openOutput(name string) (io.Writer, func(), error) {
	if name == "stdout" || name == "" {
		return os.Stdout, func() {}, nil
	}

	f, err := os.Create(name)
	if err != nil {
		return nil, nil, err
	}

	return f, func() { _ = f.Close() }, nil
}
