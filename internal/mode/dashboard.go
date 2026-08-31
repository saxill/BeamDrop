package mode

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/spool"
)

// A relay runs on a machine with no screen. Everything the TUI showed —
// who is connected, what is waiting, what arrived — becomes invisible the
// moment beamdrop is a background service on a Pi, which is exactly when
// you most need to see it.
//
// So: a page. It sits on the same plain-HTTP, tailnet-only door as the
// upload endpoint, for the same reasons — no certificate warning to click
// through, and the tailnet is already doing the encryption. It needs the
// same token, and it never displays that token: a dashboard is the kind of
// thing left open on a second monitor.

type dashboardData struct {
	Host      string
	Relay     bool
	InboxDir  string
	Peers     []dashPeer
	Spooled   []dashSpooled
	SpoolSize string
	Inbox     []dashFile
	Activity  []activityEntry
	Now       string
	Token     string // carried in links only, never rendered as text
}

type dashPeer struct {
	Name string
	Key  string
}

type dashSpooled struct {
	To, Name, Size, Waiting, LastError string
	Attempts                           int
}

type dashFile struct {
	Name, Size, Age string
}

func dashboardHandler(rt uploadRoute, registry *engine.Registry, sp *spool.Spool, act *activityLog) http.Handler {
	tmpl := template.Must(template.New("dash").Parse(dashboardHTML))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isTailnetAddr(r.RemoteAddr) {
			http.Error(w, "only reachable from the tailnet", http.StatusForbidden)
			return
		}
		if !validToken(r, rt.Token) {
			// A bare hint rather than the page: someone who reached this far
			// is on the tailnet, so telling them where the token lives is
			// useful and costs nothing.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, "add ?token=... to this URL\nthe token is in %s on this machine\n",
				filepath.Join("~/.config/beamdrop", uploadTokenFile))
			return
		}

		data := dashboardData{
			Host:     hostName(),
			Relay:    sp != nil,
			InboxDir: rt.InboxDir,
			Now:      time.Now().Format("15:04:05"),
			Token:    rt.Token,
		}
		if registry != nil {
			for _, e := range registry.All() {
				k := e.PeerPubKey()
				data.Peers = append(data.Peers, dashPeer{
					Name: e.PeerName(),
					Key:  fmt.Sprintf("%x", k[:4]),
				})
			}
			sort.Slice(data.Peers, func(a, b int) bool { return data.Peers[a].Name < data.Peers[b].Name })
		}
		if sp != nil {
			items, _ := sp.Pending()
			for _, i := range items {
				data.Spooled = append(data.Spooled, dashSpooled{
					To: i.To, Name: i.Name, Size: humanBytesUI(i.Size),
					Waiting:   shortDuration(time.Since(i.ReceivedAt)),
					Attempts:  i.Attempts,
					LastError: i.LastError,
				})
			}
			total, _ := sp.Bytes()
			data.SpoolSize = humanBytesUI(total)
		}
		data.Inbox = recentInbox(rt.InboxDir, 25)
		if act != nil {
			data.Activity = act.Recent(40)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page carries the token in its own links; a shared cache would
		// be handing that out.
		w.Header().Set("Cache-Control", "no-store")
		_ = tmpl.Execute(w, data)
	})
}

// filesHandler serves files out of the inbox, so a headless relay is not a
// place files go to be stranded.
func filesHandler(rt uploadRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isTailnetAddr(r.RemoteAddr) {
			http.Error(w, "only reachable from the tailnet", http.StatusForbidden)
			return
		}
		if !validToken(r, rt.Token) {
			http.Error(w, "bad or missing token", http.StatusUnauthorized)
			return
		}
		// The name comes from a URL a browser built, but it is still input:
		// Base keeps it inside the inbox no matter what was asked for.
		name := filepath.Base(r.URL.Query().Get("name"))
		if name == "" || name == "." || name == string(filepath.Separator) {
			http.Error(w, "no file named", http.StatusBadRequest)
			return
		}
		full := filepath.Join(rt.InboxDir, name)
		f, err := os.Open(full)
		if err != nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		http.ServeContent(w, r, name, info.ModTime(), f)
	})
}

func recentInbox(dir string, max int) []dashFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type withTime struct {
		f dashFile
		t time.Time
	}
	var all []withTime
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, withTime{
			f: dashFile{Name: e.Name(), Size: humanBytesUI(info.Size()), Age: shortDuration(time.Since(info.ModTime()))},
			t: info.ModTime(),
		})
	}
	sort.Slice(all, func(a, b int) bool { return all[a].t.After(all[b].t) })
	if len(all) > max {
		all = all[:max]
	}
	out := make([]dashFile, 0, len(all))
	for _, a := range all {
		out = append(out, a.f)
	}
	return out
}

func humanBytesUI(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// shortDuration renders an age the way a person would say it.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>beamdrop · {{.Host}}</title>
<meta http-equiv="refresh" content="10">
<style>
 :root { color-scheme: dark; }
 body { font-family: -apple-system, system-ui, sans-serif; margin: 0; padding: 1.5rem;
        background: #0d0d0f; color: #e8e8ea; line-height: 1.5; }
 .wrap { max-width: 60rem; margin: 0 auto; }
 h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
 h2 { font-size: .95rem; margin: 2rem 0 .5rem; color: #9a9aa2; font-weight: 600;
      text-transform: uppercase; letter-spacing: .04em; }
 .sub { color: #7a7a82; font-size: .85rem; margin-bottom: .5rem; }
 .pill { display: inline-block; padding: .1rem .5rem; border-radius: 999px;
         font-size: .75rem; background: #1c2b1e; color: #7fd18a; border: 1px solid #2c4430; }
 .pill.off { background: #2b1c1c; color: #d18a7f; border-color: #442c2c; }
 table { width: 100%; border-collapse: collapse; font-size: .9rem; }
 th { text-align: left; color: #7a7a82; font-weight: 500; padding: .35rem .6rem .35rem 0;
      border-bottom: 1px solid #23232a; font-size: .8rem; }
 td { padding: .4rem .6rem .4rem 0; border-bottom: 1px solid #17171c; vertical-align: top; }
 td.num { color: #9a9aa2; white-space: nowrap; }
 a { color: #6db3f2; text-decoration: none; }
 a:hover { text-decoration: underline; }
 .empty { color: #5a5a62; font-style: italic; padding: .5rem 0; }
 .err { color: #d18a7f; font-size: .8rem; }
 .log { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .8rem;
        background: #131318; border: 1px solid #23232a; border-radius: 6px;
        padding: .6rem .8rem; max-height: 20rem; overflow-y: auto; }
 .log div { padding: .1rem 0; color: #b8b8c0; }
 .log time { color: #5a5a62; margin-right: .6rem; }
 footer { margin-top: 2.5rem; color: #5a5a62; font-size: .8rem; }
</style></head><body><div class="wrap">

<h1>beamdrop · {{.Host}}</h1>
<div class="sub">
  {{if .Relay}}<span class="pill">relay</span>{{else}}<span class="pill off">not relaying</span>{{end}}
  · inbox {{.InboxDir}} · refreshed {{.Now}}
</div>

<h2>Connected now</h2>
{{if .Peers}}<table><tr><th>Peer</th><th>Key</th></tr>
{{range .Peers}}<tr><td>{{.Name}}</td><td class="num">{{.Key}}…</td></tr>{{end}}</table>
{{else}}<div class="empty">nobody connected</div>{{end}}

{{if .Relay}}
<h2>Waiting to be delivered {{if .Spooled}}({{.SpoolSize}}){{end}}</h2>
{{if .Spooled}}<table><tr><th>For</th><th>File</th><th>Size</th><th>Waiting</th><th>Tries</th></tr>
{{range .Spooled}}<tr><td>{{.To}}</td><td>{{.Name}}{{if .LastError}}<div class="err">{{.LastError}}</div>{{end}}</td>
<td class="num">{{.Size}}</td><td class="num">{{.Waiting}}</td><td class="num">{{.Attempts}}</td></tr>{{end}}</table>
{{else}}<div class="empty">nothing waiting — everything has been delivered</div>{{end}}
{{end}}

<h2>Inbox</h2>
{{if .Inbox}}<table><tr><th>File</th><th>Size</th><th>Arrived</th></tr>
{{range .Inbox}}<tr><td><a href="/files?name={{.Name}}&amp;token={{$.Token}}">{{.Name}}</a></td>
<td class="num">{{.Size}}</td><td class="num">{{.Age}}</td></tr>{{end}}</table>
{{else}}<div class="empty">nothing received yet</div>{{end}}

<h2>Activity</h2>
{{if .Activity}}<div class="log">
{{range .Activity}}<div><time>{{.At.Format "15:04:05"}}</time>{{.Text}}</div>{{end}}
</div>{{else}}<div class="empty">nothing yet</div>{{end}}

<footer>auto-refreshes every 10s</footer>
</div></body></html>`
