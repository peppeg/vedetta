// Command browsercmp reads a list of URLs with a headless browser driven over
// CDP by chromedp, for the reader comparison of R7 (docs/SPEC_R7_LETTORE_DI_RISERVA.md).
// It is a separate module so chromedp stays out of Vedetta's go.mod until a
// decision is taken.
//
// The browser is started once (as on demand in a session), the URLs are opened
// one after the other in the same tab, and the rendered HTML is saved for Vedetta's own extractor
// (scripts/htmltext). Run it in its own cgroup (systemd-run --scope) to read
// the peak memory of the browser with all its processes.
//
// Usage: browsercmp -engine chromium|lightpanda -bin PATH -urls FILE -out DIR
// Prints one JSON line per URL, then one summary line.
package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// Same user agent as Vedetta's http reader.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36 VedettaBot/1.0"

type pageResult struct {
	URL     string `json:"url"`
	File    string `json:"file,omitempty"`
	Bytes   int    `json:"bytes"`
	MS      int64  `json:"ms"`
	CloseMS int64  `json:"close_ms,omitempty"`
	Error   string `json:"error,omitempty"`
}

type summary struct {
	Engine  string `json:"engine"`
	StartMS int64  `json:"start_ms"`
	Pages   int    `json:"pages"`
	Errors  int    `json:"errors"`
	TotalMS int64  `json:"total_ms"`
}

func main() {
	engine := flag.String("engine", "chromium", "chromium or lightpanda")
	bin := flag.String("bin", "", "browser binary")
	urlsFile := flag.String("urls", "", "file with one URL per line")
	out := flag.String("out", "", "directory for the rendered HTML")
	timeout := flag.Duration("timeout", 20*time.Second, "per page")
	settle := flag.Duration("settle", 1500*time.Millisecond, "wait after the load event, for scripts")
	cdpURL := flag.String("cdp", "", "connect to a running browser at this CDP address instead of starting one (SPEC_BROWSER B.0)")
	mode := flag.String("context", "tab", "tab: all pages in one tab; page: a new browser context per page, closed after it; conn: a new CDP connection per page (-cdp only; Lightpanda allows one browser context per connection)")
	flag.Parse()
	if (*bin == "" && *cdpURL == "") || *urlsFile == "" || *out == "" || (*mode != "tab" && *mode != "page" && *mode != "conn") || (*mode == "conn" && *cdpURL == "") {
		fmt.Fprintln(os.Stderr, "usage: browsercmp -engine NAME (-bin PATH | -cdp URL) [-context tab|page] -urls FILE -out DIR")
		os.Exit(2)
	}
	urls, err := readLines(*urlsFile)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}

	start := time.Now()
	var browserCtx context.Context
	var stop func()
	if *cdpURL != "" {
		allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), *cdpURL)
		ctx, cancelCtx := chromedp.NewContext(allocCtx)
		browserCtx, stop = ctx, func() { cancelCtx(); cancelAlloc() }
	} else {
		browserCtx, stop, err = startBrowser(*engine, *bin)
	}
	if err != nil {
		fatal(err)
	}
	defer stop()
	// The first tab makes chromedp actually connect: count it in the start time.
	if err := chromedp.Run(browserCtx); err != nil {
		fatal(fmt.Errorf("browser start: %w", err))
	}
	sum := summary{Engine: *engine, StartMS: time.Since(start).Milliseconds()}

	enc := json.NewEncoder(os.Stdout)
	for _, u := range urls {
		var res pageResult
		if *mode == "conn" {
			res = readPageOwnConn(*cdpURL, u, *out, *timeout, *settle)
		} else {
			res = readPage(browserCtx, u, *out, *timeout, *settle, *mode == "page")
		}
		sum.Pages++
		if res.Error != "" {
			sum.Errors++
		}
		_ = enc.Encode(res)
	}
	sum.TotalMS = time.Since(start).Milliseconds()
	_ = enc.Encode(sum)
}

func readPage(browserCtx context.Context, u, out string, timeout, settle time.Duration, ownContext bool) (res pageResult) {
	res.URL = u
	parent := browserCtx
	if ownContext {
		// A browser context of its own (cookies, cache, memory), disposed
		// when the page is done: the "room per page" of SPEC_BROWSER §2.
		pageCtx, cancelPage := chromedp.NewContext(browserCtx, chromedp.WithNewBrowserContext())
		defer func() {
			t := time.Now()
			_ = chromedp.Cancel(pageCtx) // closes the tab and waits for it
			cancelPage()
			res.CloseMS = time.Since(t).Milliseconds()
		}()
		parent = pageCtx
	}
	// Otherwise one tab for all pages: Lightpanda serves a single page per
	// browser context in that mode, and Chromium gets the same treatment.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var html string
	t0 := time.Now()
	err := chromedp.Run(ctx,
		emulation.SetUserAgentOverride(userAgent),
		chromedp.Navigate(u),
		chromedp.Sleep(settle),
		chromedp.Evaluate(`document.documentElement.outerHTML`, &html),
	)
	res.MS = time.Since(t0).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	h := sha1.Sum([]byte(u))
	res.File = filepath.Join(out, hex.EncodeToString(h[:8])+".html")
	res.Bytes = len(html)
	if err := os.WriteFile(res.File, []byte(html), 0o644); err != nil {
		res.Error = err.Error()
	}
	return res
}

// readPageOwnConn reads one page over a CDP connection of its own, closed after it.
func readPageOwnConn(cdpURL, u, out string, timeout, settle time.Duration) pageResult {
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), cdpURL)
	defer cancelAlloc()
	connCtx, cancelConn := chromedp.NewContext(allocCtx)
	t := time.Now()
	res := readPage(connCtx, u, out, timeout, settle, false)
	_ = chromedp.Cancel(connCtx)
	cancelConn()
	res.CloseMS = time.Since(t).Milliseconds() - res.MS
	return res
}

// startBrowser launches the browser and returns a chromedp browser context.
func startBrowser(engine, bin string) (context.Context, func(), error) {
	switch engine {
	case "chromium":
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(bin),
			chromedp.UserAgent(userAgent),
			chromedp.WindowSize(1366, 900),
		)
		allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
		ctx, cancelCtx := chromedp.NewContext(allocCtx)
		return ctx, func() { cancelCtx(); cancelAlloc() }, nil
	case "lightpanda":
		port, err := freePort()
		if err != nil {
			return nil, nil, err
		}
		cmd := exec.Command(bin, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(port))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		kill := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if err := waitPort(addr, 10*time.Second); err != nil {
			kill()
			return nil, nil, err
		}
		allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), "ws://"+addr, chromedp.NoModifyURL)
		ctx, cancelCtx := chromedp.NewContext(allocCtx)
		return ctx, func() { cancelCtx(); cancelAlloc(); kill() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown engine %q", engine)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitPort(addr string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("browser did not listen on %s", addr)
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return lines, sc.Err()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "browsercmp:", err)
	os.Exit(1)
}
