package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	tracepkg "github.com/cploutarchou/gopulse/trace"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	// 1) Allow env overrides (useful for local testing or custom builds)
	if v := strings.TrimSpace(os.Getenv("GOPULSE_VERSION")); v != "" {
		version = v
	}
	if c := strings.TrimSpace(os.Getenv("GOPULSE_COMMIT")); c != "" {
		commit = c
	}
	if d := strings.TrimSpace(os.Getenv("GOPULSE_DATE")); d != "" {
		date = d
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		// Main version e.g. "v1.0.0" when installed as `go install ...@v1.0.0`
		if (version == "" || version == "dev") && info.Main.Version != "" {
			version = info.Main.Version
		}
		// Scan VCS settings for commit and time.
		var vcsRev, vcsTime string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				vcsRev = s.Value
			case "vcs.time":
				vcsTime = s.Value
			}
		}
		if (commit == "" || commit == "none") && vcsRev != "" {
			commit = vcsRev
		}
		if (date == "" || date == "unknown") && vcsTime != "" {
			// Normalize to RFC3339 if possible, else keep as-is.
			if t, err := time.Parse(time.RFC3339, vcsTime); err == nil {
				date = t.UTC().Format(time.RFC3339)
			} else {
				date = vcsTime
			}
		}
	}
}

func main() {
	log.SetFlags(0)

	// Global version flags: gopulse --version | -v
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		printVersion()
		return
	}

	if len(os.Args) < 2 {
		usage()
		return
	}

	sub := os.Args[1]
	switch sub {
	case "analyze":
		analyzeCmd(os.Args[2:])
	case "web":
		webCmd(os.Args[2:])
	case "version":
		printVersion()
	default:
		usage()
	}
}

func printVersion() {
	fmt.Printf("gopulse %s (commit %s) built %s\n", version, short(commit), date)
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func usage() {
	fmt.Println("gopulse: analyze and visualize GoPulse trace logs")
	fmt.Println("\nUsage:")
	fmt.Println("  gopulse analyze <events.log>")
	fmt.Println("  gopulse web <events.log> [-addr :8080] [-live]")
	fmt.Println("  gopulse version | --version | -v")
}

func analyzeCmd(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		log.Fatal("analyze requires an events log file path")
	}
	path := fs.Arg(0)
	events, err := readEvents(path)
	if err != nil {
		log.Fatal(err)
	}
	printSummary(events)
}

func printSummary(events []tracepkg.Event) {
	// Group by gid
	byG := map[int64][]tracepkg.Event{}
	for _, e := range events {
		byG[e.GID] = append(byG[e.GID], e)
	}
	gids := make([]int, 0, len(byG))
	for gid := range byG {
		gids = append(gids, int(gid))
	}
	sort.Ints(gids)
	fmt.Printf("Found %d goroutines in log\n\n", len(gids))
	leaked := 0
	for _, g := range gids {
		gid := int64(g)
		es := byG[gid]
		sort.Slice(es, func(i, j int) bool { return es[i].Time.Before(es[j].Time) })
		startT, stopT := time.Time{}, time.Time{}
		for _, e := range es {
			if e.Type == tracepkg.EventGStart && startT.IsZero() {
				startT = e.Time
			}
			if e.Type == tracepkg.EventGStop {
				stopT = e.Time
			}
		}
		status := "running"
		if !stopT.IsZero() {
			status = "finished"
		} else if startT.IsZero() {
			status = "observed"
		} else {
			leaked++
		}
		fmt.Printf("G#%d: %s -> %s (%s)\n", gid, ts(startT), tsOrDash(stopT), status)
		// show top 5 events
		max_ := 5
		if len(es) < max_ {
			max_ = len(es)
		}
		for i := 0; i < max_; i++ {
			fmt.Printf("  - %s %s", es[i].Time.Format(time.RFC3339Nano), es[i].Type)
			if es[i].ChanID != "" {
				fmt.Printf(" chan=%s", es[i].ChanID)
			}
			fmt.Println()
		}
	}
	fmt.Printf("\nPotential leaks (goroutines without stop): %d\n", leaked)

	// crude deadlock hint: if last event older than 5s and leaks > 0
	if len(events) > 0 {
		last := events[len(events)-1].Time
		if time.Since(last) > 5*time.Second && leaked > 0 {
			fmt.Println("\nHint: Events are stale and some goroutines haven't stopped; possible deadlock.")
		}
	}
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339Nano)
}
func tsOrDash(t time.Time) string { return ts(t) }

func webCmd(args []string) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	live := fs.Bool("live", false, "enable live streaming (tail the file)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		log.Fatal("web requires an events log file path")
	}
	path := fs.Arg(0)
	if *live {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(liveIndexHTML))
		})
		http.HandleFunc("/stream", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			fl, ok := w.(http.Flusher)
			if !ok {
				w.WriteHeader(500)
				return
			}

			// send backlog
			evs, err := readEvents(path)
			if err == nil {
				for _, e := range evs {
					b, _ := json.Marshal(e)
					fmt.Fprintf(w, "data: %s\n\n", string(b))
				}
				fl.Flush()
			}
			// tail new lines
			f, err := os.Open(path)
			if err != nil {
				return
			}
			defer f.Close()
			offset, _ := f.Seek(0, 2)
			rdr := bufio.NewReader(f)
			notify := req.Context().Done()
			for {
				select {
				case <-notify:
					return
				default:
					stat, err := f.Stat()
					if err != nil {
						time.Sleep(200 * time.Millisecond)
						continue
					}
					if stat.Size() < offset {
						offset, _ = f.Seek(0, 0)
						rdr.Reset(f)
					}
					if stat.Size() == offset {
						time.Sleep(200 * time.Millisecond)
						continue
					}
					line, err := rdr.ReadString('\n')
					if err != nil {
						time.Sleep(100 * time.Millisecond)
						continue
					}
					offset += int64(len(line))
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					var e tracepkg.Event
					if json.Unmarshal([]byte(line), &e) == nil {
						b, _ := json.Marshal(e)
						fmt.Fprintf(w, "data: %s\n\n", string(b))
						fl.Flush()
					}
				}
			}
		})
	} else {
		http.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
			events, err := readEvents(path)
			if err != nil {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(events)
		})
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(indexHTML))
		})
	}
	log.Printf("Serving web UI on %s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func readEvents(path string) ([]tracepkg.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var out []tracepkg.Event
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var e tracepkg.Event
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			out = append(out, e)
		}
	}
	return out, s.Err()
}

const indexHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <title>GoPulse Web UI</title>
  <style>
    body { font-family: -apple-system, Segoe UI, Roboto, sans-serif; margin: 20px; }
    .row { margin-bottom: 8px; }
    .bar { height: 14px; background: #4F46E5; display: inline-block; }
    .axis { color: #666; font-size: 12px; }
    .gid { width: 80px; display: inline-block; }
  </style>
</head>
<body>
  <h1>GoPulse: Goroutine Timeline</h1>
  <p>Simple visualization of goroutine start/stop and channel events. Refresh page to reload events.</p>
  <div id="chart"></div>
  <script>
  async function main(){
    const res = await fetch('/events');
    const events = await res.json();
    // group by gid
    const byG = new Map();
    for (const e of events){
      if (!byG.has(e.gid)) byG.set(e.gid, []);
      byG.get(e.gid).push(e);
    }
    const container = document.getElementById('chart');
    container.innerHTML = '';
    const rows = [];
    for (const [gid, es] of byG.entries()){
      es.sort((a,b)=> new Date(a.time)-new Date(b.time));
      const start = es.find(e=>e.type==='g_start')?.time;
      const stop = [...es].reverse().find(e=>e.type==='g_stop')?.time;
      const row = document.createElement('div');
      row.className = 'row';
      const gidEl = document.createElement('span');
      gidEl.className = 'gid'; gidEl.textContent = 'G#'+gid;
      const bar = document.createElement('span');
      bar.className = 'bar';
      const t0 = start ? new Date(start).getTime() : new Date(es[0].time).getTime();
      const t1 = stop ? new Date(stop).getTime() : new Date(es[es.length-1].time).getTime();
      const width = Math.max(2, (t1 - t0) / 5); // 5ms -> 1px scaling
      bar.style.width = width + 'px';
      row.appendChild(gidEl); row.appendChild(bar);
      // annotate channel events
      const notes = document.createElement('span');
      notes.className = 'axis';
      const cEvents = es.filter(e => e.type==='c_send' || e.type==='c_recv' || e.type==='c_close');
      notes.textContent = '  ' + cEvents.map(e=>e.type + (e.chan_id?('('+e.chan_id+')'):'')).join(', ');
      row.appendChild(notes);
      container.appendChild(row);
    }
  }
  main();
  </script>
</body>
</html>`

const liveIndexHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <title>GoPulse Live</title>
  <style>
    body { font-family: -apple-system, Segoe UI, Roboto, sans-serif; margin: 20px; }
    h2 { margin-top: 20px; }
    table { border-collapse: collapse; width: 100%; margin-top: 8px; }
    th, td { border-bottom: 1px solid #eee; padding: 6px 8px; text-align: left; }
    .status-running { color: #2563EB; }
    .status-finished { color: #059669; }
    .status-observed { color: #6B7280; }
    .status-blocked { color: #F59E0B; font-weight: 600; }
    .pill { display:inline-block; padding: 2px 6px; border-radius: 10px; background:#F3F4F6; font-size: 12px; margin-right:4px; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    #summary { margin: 12px 0; color: #374151; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 8px; }
    .card { background: #F9FAFB; border: 1px solid #E5E7EB; border-radius: 8px; padding: 10px; }
  </style>
</head>
<body>
  <h1>GoPulse Live (CLI)</h1>
  <p>Streaming events from log file. This page updates automatically.</p>

  <div id="summary" class="grid">
    <div class="card"><b>Total events:</b> <span id="sum-events">0</span></div>
    <div class="card"><b>Total goroutines:</b> <span id="sum-g">0</span></div>
    <div class="card"><b>Running:</b> <span id="sum-running">0</span></div>
    <div class="card"><b>Blocked:</b> <span id="sum-blocked">0</span></div>
    <div class="card"><b>Finished:</b> <span id="sum-finished">0</span></div>
    <div class="card"><b>Observed:</b> <span id="sum-observed">0</span></div>
  </div>

  <h2>Goroutines</h2>
  <table>
    <thead>
      <tr><th>Goroutine</th><th>Status</th><th>Duration</th><th>Sends</th><th>Receives</th><th>Closes</th><th>Last Event</th></tr>
    </thead>
    <tbody id="tbody"></tbody>
  </table>

  <h2>Channels</h2>
  <table>
    <thead>
      <tr><th>Channel</th><th>Sends</th><th>Receives</th><th>Closes</th></tr>
    </thead>
    <tbody id="chbody"></tbody>
  </table>

  <script>
    const state = new Map();
    const channels = new Map();
    let totalEvents = 0;

    function upsert(e){
      totalEvents++;
      const gid = e.gid; let s = state.get(gid) || {sends:0,recvs:0,closes:0};
      const t = new Date(e.time); s.lastTime = t;
      if(e.type==='g_start'){ s.start=t; }
      if(e.type==='g_stop'){ s.stop=t; s.blocked=false; s.blockReason=undefined; }
      if(e.type==='block'){ s.blocked=true; try{ s.blockReason=(e.details && e.details.reason)||''; }catch(_){} }
      if(e.type==='unblock'){ s.blocked=false; }
      if(e.type==='c_send'){ s.sends++; touchChan(e.chan_id,'sends'); }
      if(e.type==='c_recv'){ s.recvs++; touchChan(e.chan_id,'recvs'); }
      if(e.type==='c_close'){ s.closes++; touchChan(e.chan_id,'closes'); }
      state.set(gid, s);
    }

    function touchChan(id, field){ if(!id) return; let c = channels.get(id) || {sends:0, recvs:0, closes:0}; c[field]++; channels.set(id, c); }
    function statusOf(s){ if(s.blocked) return ['blocked'+(s.blockReason?(' ('+s.blockReason+')'):''),'status-blocked']; if(s.stop) return ['finished','status-finished']; if(s.start) return ['running','status-running']; return ['observed','status-observed']; }

    function render(){
      let running=0, finished=0, observed=0, blocked=0;
      for(const s of state.values()){
        if(s.blocked) blocked++; else if(s.stop) finished++; else if(s.start) running++; else observed++;
      }
      document.getElementById('sum-events').textContent = String(totalEvents);
      document.getElementById('sum-g').textContent = String(state.size);
      document.getElementById('sum-running').textContent = String(running);
      document.getElementById('sum-blocked').textContent = String(blocked);
      document.getElementById('sum-finished').textContent = String(finished);
      document.getElementById('sum-observed').textContent = String(observed);

      const tbody=document.getElementById('tbody'); tbody.innerHTML='';
      const gids = Array.from(state.keys()).sort((a,b)=>a-b);
      for(const gid of gids){ const s=state.get(gid); const tr=document.createElement('tr'); const [label, cls]=statusOf(s);
        const dur = s.start? ((s.stop||new Date())-s.start):0;
        tr.innerHTML = '<td class="mono">G#'+gid+'</td>'+
          '<td class="'+cls+'">'+label+'</td>'+
          '<td>'+(s.start? (dur/1000).toFixed(3)+'s':'-')+'</td>'+
          '<td><span class="pill">'+s.sends+'</span></td>'+
          '<td><span class="pill">'+s.recvs+'</span></td>'+
          '<td><span class="pill">'+s.closes+'</span></td>'+
          '<td class="mono">'+(s.lastTime? s.lastTime.toISOString():'-')+'</td>';
        tbody.appendChild(tr);
      }

      const chbody=document.getElementById('chbody'); chbody.innerHTML='';
      const ids = Array.from(channels.keys()).sort();
      for(const id of ids){ const c = channels.get(id); const tr=document.createElement('tr');
        tr.innerHTML = '<td class="mono">'+id+'</td>'+
          '<td><span class="pill">'+c.sends+'</span></td>'+
          '<td><span class="pill">'+c.recvs+'</span></td>'+
          '<td><span class="pill">'+c.closes+'</span></td>';
        chbody.appendChild(tr);
      }
    }

    const es = new EventSource('/stream');
    es.onmessage = (msg)=>{ try{ upsert(JSON.parse(msg.data)); render(); }catch(e){} };
  </script>
</body>
</html>`
