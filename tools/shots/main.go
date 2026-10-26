// Command shots renders the README screenshots. Screen tests write their
// golden frames with colors when RM_SHOTS names a directory; shots turns those
// frames into terminal windows (HTML) and has headless Chrome photograph them.
//
//	make shots
package main

import (
	"flag"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Cell metrics in CSS pixels: JetBrains Mono advances 0.6em.
const (
	fontPx  = 15.0
	cellW   = fontPx * 0.6
	cellH   = 19.0
	padX    = 18.0
	padY    = 14.0
	barH    = 40.0
	scale   = 1.5
	defFG   = "#c9d4e5"
	termBG  = "#0b1120"
	heroW   = 1640.0
	heroTop = 300.0
)

// shot is one feature screenshot: a frame and its window title.
type shot struct{ file, frame, title string }

var shots = []shot{
	{"home", "home_home_full_100", "Home"},
	{"changeset", "changeset_changeset_80", "Change set"},
	{"run", "run_run_db_80", "Run"},
	{"rotate", "rotate_rotate_80", "Rotate secrets"},
	{"password", "rotate_password_places_80", "Rotate · mongo root"},
	{"services", "services_services_panel_80", "Services"},
	{"variables", "variables_variables_tree_value_80", "Variables"},
	{"history", "history_history_80", "History"},
	{"settings", "settings_settings_full_100", "Settings"},
}

func main() {
	in := flag.String("in", "", "directory of .ansi frames written by RM_SHOTS")
	out := flag.String("out", "docs/assets/shots", "output directory")
	chrome := flag.String("chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "Chrome binary")
	flag.Parse()
	if *in == "" {
		log.Fatal("shots: -in is required")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	tmp, err := os.MkdirTemp("", "shots")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	load := func(name string) [][]cell {
		b, err := os.ReadFile(filepath.Join(*in, name+".ansi"))
		if err != nil {
			log.Fatal(err)
		}
		return squeeze(parse(string(b)))
	}

	for _, s := range shots {
		g := load(s.frame)
		w, h := winSize(g)
		page := pageHTML(w+80, h+80, false, window(g, s.title, 40, 34))
		photo(*chrome, tmp, filepath.Join(*out, s.file+".png"), page, w+80, h+80, true)
	}

	mon, rot, cs := load("services_services_monitor_150"), load("rotate_rotate_80"), load("changeset_changeset_80")
	_, mh := winSize(mon)
	rw, rh := winSize(rot)
	_, ch := winSize(cs)
	csY, rotY := heroTop+mh-70, heroTop+mh-30
	body := heroHeader() +
		window(mon, "Services", (heroW-1386-2*padX)/2, heroTop) +
		window(cs, "Change set", 24, csY) +
		window(rot, "Rotate secrets", heroW-rw-24, rotY)
	hh := max(csY+ch, rotY+rh) + 44
	photo(*chrome, tmp, filepath.Join(*out, "hero.png"), pageHTML(heroW, hh, true, body), heroW, hh, false)
}

// photo writes page to a temp file and screenshots it at w×h CSS pixels.
func photo(chrome, tmp, dst, page string, w, h float64, transparent bool) {
	src := filepath.Join(tmp, filepath.Base(dst)+".html")
	if err := os.WriteFile(src, []byte(page), 0o644); err != nil {
		log.Fatal(err)
	}
	abs, _ := filepath.Abs(dst)
	args := []string{
		"--headless=new", "--hide-scrollbars", "--disable-gpu",
		"--force-device-scale-factor=" + strconv.FormatFloat(scale, 'f', -1, 64),
		fmt.Sprintf("--window-size=%d,%d", int(w), int(h)),
		"--virtual-time-budget=8000",
		"--screenshot=" + abs,
	}
	if transparent {
		args = append(args, "--default-background-color=00000000")
	}
	args = append(args, "file://"+src)
	if out, err := exec.Command(chrome, args...).CombinedOutput(); err != nil {
		log.Fatalf("chrome %s: %v\n%s", dst, err, out)
	}
	fmt.Println("wrote", dst)
}

// winSize is a window's outer size for a grid.
func winSize(g [][]cell) (float64, float64) {
	cols := 0
	for _, l := range g {
		cols = max(cols, len(l))
	}
	return float64(cols)*cellW + 2*padX, float64(len(g))*cellH + 2*padY + barH
}

func pageHTML(w, h float64, hero bool, body string) string {
	bg := "transparent"
	if hero {
		bg = "radial-gradient(1200px 700px at 50% 0%, #16243f 0%, #0a1020 55%, #060a14 100%)"
	}
	return `<!doctype html><html><head><meta charset="utf-8">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:ital,wght@0,400;0,700;1,400&display=block" rel="stylesheet">
<style>
html,body{margin:0;padding:0}
body{width:` + px(w) + `;height:` + px(h) + `;background:` + bg + `;position:relative;overflow:hidden;font-family:'JetBrains Mono',monospace}
.win{position:absolute;border-radius:14px;background:` + termBG + `;border:1px solid rgba(140,170,220,.16);box-shadow:0 24px 60px rgba(0,0,0,.55),0 4px 14px rgba(0,0,0,.35);overflow:hidden}
.bar{height:` + px(barH) + `;display:flex;align-items:center;padding:0 16px;background:linear-gradient(#141d31,#101827);border-bottom:1px solid rgba(140,170,220,.10);color:#7d8aa3;font-size:14px}
.dot{width:12px;height:12px;border-radius:50%;margin-right:8px}
.bar span.t{margin-left:14px}
.bg{position:absolute;height:` + px(cellH) + `}
pre{position:relative;margin:0;padding:` + px(padY) + ` ` + px(padX) + `;font:` + px(fontPx) + `/` + px(cellH) + ` 'JetBrains Mono',monospace;color:` + defFG + `;font-variant-ligatures:none}
pre span.c{display:inline-block;width:` + px(cellW) + `;height:` + px(cellH) + `;line-height:` + px(cellH) + `;vertical-align:top;text-align:center;overflow:visible}
pre span.b{transform:scale(1.25,1.45)}
pre span.k{transform:scale(1.02,1.3)}
.hero{position:absolute;left:0;right:0;top:46px;text-align:center;color:#e8f1ff}
.hero .row{display:flex;align-items:center;justify-content:center;gap:26px}
.hero h1{margin:0;font-size:88px;font-weight:700;letter-spacing:-2px;background:linear-gradient(90deg,#5fd787,#00d7ff 60%,#00afff);-webkit-background-clip:text;background-clip:text;color:transparent}
.hero .tag{margin-top:14px;font-size:28px;font-style:italic;color:#9fb0cc}
.hero .plat{margin-top:14px;font-size:20px;letter-spacing:6px;color:#5d6b85}
</style></head><body>` + body + `</body></html>`
}

func heroHeader() string {
	icon, err := os.ReadFile("docs/assets/icon.svg")
	if err != nil {
		log.Fatal(err)
	}
	svg := strings.Replace(string(icon), `width="512" height="512"`, `width="112" height="112"`, 1)
	return `<div class="hero"><div class="row">` + svg + `<h1>railway-manager</h1></div>
<div class="tag">stage · review · apply · resume</div>
<div class="plat">macos · linux</div></div>`
}

// window renders a grid as a terminal window at (x, y).
func window(g [][]cell, title string, x, y float64) string {
	w, h := winSize(g)
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="win" style="left:%s;top:%s;width:%s;height:%s">`, px(x), px(y), px(w), px(h))
	b.WriteString(`<div class="bar"><span class="dot" style="background:#ff5f57"></span><span class="dot" style="background:#febc2e"></span><span class="dot" style="background:#28c840"></span>`)
	fmt.Fprintf(&b, `<span class="t">~ — railway-manager · %s</span></div>`, html.EscapeString(title))
	// Backgrounds as rectangles on the cell grid: inline backgrounds bleed
	// into the row above where a fallback font is taller than the cell.
	for i, l := range g {
		for j := 0; j < len(l); {
			_, bg := l[j].st.colors()
			k := j + 1
			for k < len(l) {
				if _, b2 := l[k].st.colors(); b2 != bg {
					break
				}
				k++
			}
			if bg != "" {
				fmt.Fprintf(&b, `<div class="bg" style="left:%s;top:%s;width:%s;background:%s"></div>`,
					px(padX+float64(j)*cellW), px(barH+padY+float64(i)*cellH), px(float64(k-j)*cellW), bg)
			}
			j = k
		}
	}
	b.WriteString(`<pre>`)
	for i, l := range g {
		if i > 0 {
			b.WriteByte('\n')
		}
		for j := 0; j < len(l); {
			st := l[j].st
			k := j
			if l[j].r < 0x80 {
				for k < len(l) && l[k].r < 0x80 && l[k].st == st {
					k++
				}
			} else {
				k = j + 1
			}
			text := make([]rune, 0, k-j)
			for _, c := range l[j:k] {
				text = append(text, c.r)
			}
			class := ""
			switch r := l[j].r; {
			case r >= 0x2800 && r <= 0x28ff: // braille charts: the fallback font draws them small
				class = ` class="c b"`
			case r >= 0x2580 && r <= 0x259f: // block elements fill the whole cell, as in a terminal
				class = ` class="c k"`
			case r >= 0x80:
				class = ` class="c"`
			}
			css := st.css()
			if class == "" && css == "" {
				b.WriteString(html.EscapeString(string(text)))
			} else {
				fmt.Fprintf(&b, `<span%s style="%s">%s</span>`, class, css, html.EscapeString(string(text)))
			}
			j = k
		}
	}
	b.WriteString(`</pre></div>`)
	return b.String()
}

func px(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "px" }

// squeeze drops body rows that are empty inside the frame's border, keeping
// one, so the screenshots are not mostly blank space.
func squeeze(g [][]cell) [][]cell {
	var out [][]cell
	blank := 0
	for i, l := range g {
		if i > 0 && i < len(g)-1 && isBlank(l) {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return out
}

func isBlank(l []cell) bool {
	if len(l) < 2 {
		return true
	}
	for _, c := range l[1 : len(l)-1] {
		if c.r != ' ' {
			return false
		}
	}
	return true
}
