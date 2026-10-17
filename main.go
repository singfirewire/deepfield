package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const appName = "DEEPFIELD"
const appVersion = "1.0.0"

//go:embed ui.html
var uiHTML []byte

// ---------- tree ----------

type Node struct {
	Name     string  `json:"n"`
	Size     int64   `json:"s"`
	IsDir    bool    `json:"d,omitempty"`
	Dom      string  `json:"k,omitempty"` // dominant content category (dirs)
	Err      string  `json:"e,omitempty"`
	Children []*Node `json:"c,omitempty"`
}

// ---------- content categories (mirrored in ui.html) ----------

var catKeys = []string{"video", "image", "audio", "archive", "code", "docs", "system", "other"}

var catExt = map[string]string{}

func init() {
	table := map[string]string{
		"video":   "mp4 mkv avi mov wmv flv webm m4v mpg mpeg ts m2ts mts vob 3gp ogv",
		"image":   "jpg jpeg png gif webp svg bmp tif tiff heic heif psd psb ico cr2 cr3 crw nef arw dng orf rw2 raf pef srw 3fr x3f rwl mrw nrw sr2 af afphoto xcf",
		"audio":   "mp3 wav flac aac ogg m4a wma opus aiff ape alac mid midi",
		"archive": "zip rar 7z tar gz bz2 xz zst iso apk cab msi jar wim esd vhd vhdx vmdk vdi qcow2 img dmg gho",
		"code":    "js mjs cjs ts tsx jsx go py c cpp cc h hpp cs rs java kt kts html htm css scss sass less json xml yml yaml toml md mdx sql sh bash zsh bat cmd ps1 vue svelte php rb swift dart lua pl r m mm ipynb lock map",
		"docs":    "pdf doc docx xls xlsx ppt pptx txt rtf csv epub odt ods odp pages numbers key",
		"system":  "exe dll sys drv dat bin log tmp db sqlite sqlite3 pak asar node pdb lib obj o class so dylib elf mui cpl ocx tlb manifest cat inf",
	}
	for k, list := range table {
		for _, e := range strings.Fields(list) {
			catExt[e] = k
		}
	}
}

func categorize(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return "other"
	}
	if k, ok := catExt[strings.ToLower(name[i+1:])]; ok {
		return k
	}
	return "other"
}

type catCounts [8]int64

func catIndex(k string) int {
	for i, v := range catKeys {
		if v == k {
			return i
		}
	}
	return len(catKeys) - 1
}

func bestCat(c catCounts) string {
	best := 0
	for i := 1; i < len(c); i++ {
		if c[i] > c[best] {
			best = i
		}
	}
	return catKeys[best]
}

// domPass tags every directory with the category that occupies most bytes
// beneath it, returning per-category byte totals up the tree.
func domPass(n *Node) catCounts {
	var c catCounts
	if n.IsDir && len(n.Children) > 0 {
		for _, ch := range n.Children {
			cc := domPass(ch)
			for i := range c {
				c[i] += cc[i]
			}
		}
	} else {
		c[catIndex(categorize(n.Name))] = n.Size
	}
	if n.IsDir {
		n.Dom = bestCat(c)
	}
	return c
}

type dirEntry struct {
	name    string
	size    int64
	isDir   bool
	reparse bool
}

type Stats struct {
	Root      string `json:"root"`
	Files     int64  `json:"files"`
	Dirs      int64  `json:"dirs"`
	Bytes     int64  `json:"bytes"`
	Errors    int64  `json:"errors"`
	ElapsedMs int64  `json:"elapsedMs"`
	Workers   int    `json:"workers"`
	Scanned   string `json:"scanned"`
	Version   string `json:"version"`
}

type ScanResult struct {
	Tree  *Node
	Stats Stats
}

// ---------- scanner ----------

type Scanner struct {
	sem    chan struct{}
	files  int64
	dirs   int64
	bytes  int64
	errs   int64
	mu     sync.Mutex
	errLog []string
}

func NewScanner(workers int) *Scanner {
	if workers <= 0 {
		workers = runtime.NumCPU() * 6
	}
	if workers > 512 {
		workers = 512
	}
	return &Scanner{sem: make(chan struct{}, workers)}
}

func (s *Scanner) logErr(path string, err error) {
	s.mu.Lock()
	if len(s.errLog) < 20 {
		s.errLog = append(s.errLog, path+": "+shortErr(err))
	}
	s.mu.Unlock()
}

func (s *Scanner) scan(dir string) *Node {
	name := baseName(dir)
	entries, err := readDirFast(dir)
	if err != nil {
		atomic.AddInt64(&s.errs, 1)
		s.logErr(dir, err)
		return &Node{Name: name, IsDir: true, Err: shortErr(err)}
	}
	atomic.AddInt64(&s.dirs, 1)

	node := &Node{Name: name, IsDir: true, Children: make([]*Node, 0, len(entries))}
	var wg sync.WaitGroup

	for _, e := range entries {
		if e.isDir {
			if e.reparse { // junction / symlink: don't follow (loops, OneDrive)
				continue
			}
			child := &Node{Name: e.name, IsDir: true}
			node.Children = append(node.Children, child)
			cp := joinPath(dir, e.name)
			// Bounded parallelism: spawn only while a slot is free, otherwise
			// recurse inline. Never blocks => never deadlocks, and the number
			// of live goroutines stays capped by the semaphore size.
			select {
			case s.sem <- struct{}{}:
				wg.Add(1)
				go func(c *Node, p string) {
					defer func() { <-s.sem; wg.Done() }()
					*c = *s.scan(p)
				}(child, cp)
			default:
				*child = *s.scan(cp)
			}
		} else {
			atomic.AddInt64(&s.files, 1)
			atomic.AddInt64(&s.bytes, e.size)
			node.Children = append(node.Children, &Node{Name: e.name, Size: e.size})
		}
	}
	wg.Wait()

	var total int64
	for _, c := range node.Children {
		total += c.Size
	}
	node.Size = total
	return node
}

func (s *Scanner) Run(root string) ScanResult {
	start := time.Now()
	tree := s.scan(root)
	el := time.Since(start)
	return ScanResult{
		Tree: tree,
		Stats: Stats{
			Root:      root,
			Files:     atomic.LoadInt64(&s.files),
			Dirs:      atomic.LoadInt64(&s.dirs),
			Bytes:     atomic.LoadInt64(&s.bytes),
			Errors:    atomic.LoadInt64(&s.errs),
			ElapsedMs: el.Milliseconds(),
			Workers:   cap(s.sem),
			Scanned:   time.Now().Format("2006-01-02 15:04:05"),
			Version:   appVersion,
		},
	}
}

func (s *Scanner) ErrLog() []string { s.mu.Lock(); defer s.mu.Unlock(); return s.errLog }

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	if strings.HasSuffix(dir, `\`) || strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

func shortErr(err error) string {
	m := err.Error()
	if len(m) > 90 {
		m = m[:90] + "…"
	}
	return m
}

// ---------- prune (keeps the browser payload sane) ----------

func prune(n *Node, budget *int, maxKids, depth int) {
	if budget != nil && *budget <= 0 {
		n.Children = nil
		return
	}
	if budget != nil {
		*budget--
	}
	if len(n.Children) == 0 {
		return
	}
	sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Size > n.Children[j].Size })
	if len(n.Children) > maxKids {
		var rest int64
		var cc catCounts
		cnt := 0
		for _, c := range n.Children[maxKids:] {
			rest += c.Size
			cnt++
			k := c.Dom
			if k == "" {
				k = categorize(c.Name)
			}
			cc[catIndex(k)] += c.Size
			if budget != nil {
				*budget--
			}
		}
		n.Children = append(n.Children[:maxKids], &Node{
			Name:  fmt.Sprintf("(+%d รายการ)", cnt),
			Size:  rest,
			IsDir: true,
			Dom:   bestCat(cc),
		})
	}
	if depth > 60 {
		n.Children = nil
		return
	}
	for _, c := range n.Children {
		if c.IsDir {
			prune(c, budget, maxKids, depth+1)
		} else if budget != nil {
			*budget--
		}
	}
}

// ---------- formatting ----------

func human(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(b)
	i := -1
	for v >= u && i < len(units)-1 {
		v /= u
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func pct(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func bar(p float64, width int) string {
	n := int(p / 100 * float64(width))
	if n < 0 {
		n = 0
	}
	if n > width {
		n = width
	}
	return strings.Repeat("█", n) + strings.Repeat("·", width-n)
}

// ---------- flatten / top ----------

type flat struct {
	path  string
	size  int64
	isDir bool
}

func flatten(n *Node, prefix string, out *[]flat) {
	for _, c := range n.Children {
		p := joinPath(prefix, c.Name)
		if c.IsDir {
			flatten(c, p, out)
		}
		*out = append(*out, flat{path: p, size: c.Size, isDir: c.IsDir})
	}
}

func topFiles(tree *Node, n int) []flat {
	var all []flat
	flatten(tree, tree.Name, &all)
	files := all[:0]
	for _, f := range all {
		if !f.isDir {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	if len(files) > n {
		files = files[:n]
	}
	return files
}

// ---------- commands ----------

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "-v", "--version":
			fmt.Printf("%s v%s (%s/%s)\n", appName, appVersion, runtime.GOOS, runtime.GOARCH)
			return
		case "help", "-h", "--help":
			usage()
			return
		case "top":
			cmdTop(args[1:])
			return
		case "scan":
			cmdScan(args[1:])
			return
		}
	}
	root := "."
	port := 7788
	open := true
	workers := 0
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--port" && i+1 < len(args):
			i++
			port, _ = strconv.Atoi(args[i])
		case strings.HasPrefix(a, "--port="):
			port, _ = strconv.Atoi(strings.TrimPrefix(a, "--port="))
		case a == "--no-open":
			open = false
		case a == "--workers" && i+1 < len(args):
			i++
			workers, _ = strconv.Atoi(args[i])
		case strings.HasPrefix(a, "--workers="):
			workers, _ = strconv.Atoi(strings.TrimPrefix(a, "--workers="))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) > 0 {
		root = rest[0]
	}
	cmdServe(root, port, open, workers)
}

func usage() {
	fmt.Printf(`%s v%s — disk space analyzer ที่เร็วกว่าเดิม

  deepfield [path]              สแกนแล้วเปิดหน้าเว็บ (ค่าเริ่มต้น = โฟลเดอร์ปัจจุบัน)
      --port 7788               เปลี่ยนพอร์ต
      --no-open                 ไม่ต้องเปิดเบราว์เซอร์
      --workers N               จำนวน worker (ค่าเริ่มต้น = CPU x6)

  deepfield top [path] -n 20    ไล่ไฟล์ใหญ่สุดแบบข้อความ
  deepfield scan [path]         พิมพ์ JSON ออก stdout
  deepfield version | help
`, appName, appVersion)
}

func resolveRoot(p string) string {
	if p == "" {
		p = "."
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	// keep "C:\" intact instead of collapsing to "C:"
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	return p
}

func cmdServe(root string, port int, openBrowser bool, workers int) {
	root = resolveRoot(root)
	if st, err := os.Stat(root); err != nil {
		fmt.Fprintf(os.Stderr, "ไม่พบโฟลเดอร์: %s\n", root)
		os.Exit(1)
	} else if !st.IsDir() {
		fmt.Fprintf(os.Stderr, "แผงนี้ต้องเป็นโฟลเดอร์ ไม่ใช่ไฟล์: %s\n", root)
		os.Exit(1)
	}

	fmt.Printf("\n  %s v%s\n", appName, appVersion)
	fmt.Printf("  กำลังสแกน %s\n", root)
	sc := NewScanner(workers)
	res := sc.Run(root)
	st := res.Stats

	tops := topFiles(res.Tree, 8)
	fmt.Printf("  เสร็จใน %s · %s · %d ไฟล์ · %d โฟลเดอร์ · %.0f ไฟล์/วิ (worker %d)\n\n",
		time.Duration(st.ElapsedMs)*time.Millisecond, human(st.Bytes), st.Files, st.Dirs,
		float64(st.Files)/maxf(float64(st.ElapsedMs)/1000, 0.001), st.Workers)

	fmt.Printf("  ใหญ่สุด 8 อันดับแรก\n")
	for i, f := range tops {
		fmt.Printf("  %2d. %9s  %s\n", i+1, human(f.size), f.path)
	}
	if st.Errors > 0 {
		fmt.Printf("\n  ข้าม %d ตำแหน่งที่อ่านไม่ได้ (สิทธิ์/hardlink)\n", st.Errors)
	}

	// domPass must run on the FULL tree, before prune: prune turns
	// budget-exhausted directories into leaves, and a leaf directory can only
	// be categorised by its own name (usually no extension => "other").
	domPass(res.Tree)
	prune(res.Tree, intp(120000), 160, 0)
	payload, err := json.Marshal(map[string]any{"meta": st, "tree": res.Tree})
	if err != nil {
		fmt.Fprintf(os.Stderr, "สร้าง JSON ไม่สำเร็จ: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(uiHTML)
	})
	mux.HandleFunc("/api/tree", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(payload)
	})
	mux.HandleFunc("/api/meta", func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(st)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/api/reveal", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		abs, err := filepath.Abs(p)
		if p == "" || err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(strings.ToLower(abs), strings.ToLower(st.Root)) {
			http.Error(w, "outside scanned root", http.StatusForbidden)
			return
		}
		exec.Command("explorer", "/select,"+abs).Start()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "พอร์ต %d ถูกใช้อยู่ — ลอง --port 7799\n", port)
		os.Exit(1)
	}
	url := "http://" + addr
	fmt.Printf("\n  เปิดใช้งานที่  %s\n", url)
	fmt.Printf("  กด Ctrl+C เพื่อปิด\n\n")
	if openBrowser {
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func cmdTop(args []string) {
	root := "."
	n := 20
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-n" || a == "--n") && i+1 < len(args):
			i++
			n, _ = strconv.Atoi(args[i])
		case strings.HasPrefix(a, "-n="):
			n, _ = strconv.Atoi(strings.TrimPrefix(a, "-n="))
		default:
			root = a
		}
	}
	root = resolveRoot(root)
	res := (&Scanner{sem: make(chan struct{}, runtime.NumCPU()*6)}).Run(root)
	list := topFiles(res.Tree, n)
	fmt.Printf("%s — %s · %d ไฟล์ · %s\n\n", root, human(res.Stats.Bytes), res.Stats.Files,
		(time.Duration(res.Stats.ElapsedMs) * time.Millisecond).String())
	for i, f := range list {
		fmt.Printf("%2d. %9s  %5.2f%%  %s\n", i+1, human(f.size), pct(f.size, res.Stats.Bytes), f.path)
	}
}

func cmdScan(args []string) {
	root := "."
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			root = a
		}
	}
	root = resolveRoot(root)
	res := (&Scanner{sem: make(chan struct{}, runtime.NumCPU()*6)}).Run(root)
	domPass(res.Tree)
	prune(res.Tree, intp(200000), 200, 0)
	out := map[string]any{"meta": res.Stats, "tree": res.Tree}
	b, _ := json.Marshal(out)
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
}

func intp(v int) *int { return &v }

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
