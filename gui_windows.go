//go:build windows

package main

// Native Win32 GUI (no browser, no CGO): one window, GDI-drawn toolbar,
// breadcrumb, treemap and status bar. All drawing goes through a memory DC so
// hover repaints don't flicker.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// ---------- win32 plumbing ----------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassExW  = user32.NewProc("RegisterClassExW")
	pCreateWindowExW   = user32.NewProc("CreateWindowExW")
	pDefWindowProcW    = user32.NewProc("DefWindowProcW")
	pShowWindow        = user32.NewProc("ShowWindow")
	pUpdateWindow      = user32.NewProc("UpdateWindow")
	pGetMessageW       = user32.NewProc("GetMessageW")
	pTranslateMessage  = user32.NewProc("TranslateMessage")
	pDispatchMessageW  = user32.NewProc("DispatchMessageW")
	pPostQuitMessage   = user32.NewProc("PostQuitMessage")
	pPostMessageW      = user32.NewProc("PostMessageW")
	pBeginPaint        = user32.NewProc("BeginPaint")
	pEndPaint          = user32.NewProc("EndPaint")
	pInvalidateRect    = user32.NewProc("InvalidateRect")
	pGetClientRect     = user32.NewProc("GetClientRect")
	pLoadCursorW       = user32.NewProc("LoadCursorW")
	pLoadIconW         = user32.NewProc("LoadIconW")
	pSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	pSetTimer          = user32.NewProc("SetTimer")
	pKillTimer         = user32.NewProc("KillTimer")
	pSetWindowTextW    = user32.NewProc("SetWindowTextW")
	pGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	pSetCapture        = user32.NewProc("SetCapture")
	pReleaseCapture    = user32.NewProc("ReleaseCapture")

	pCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	pAppendMenuW       = user32.NewProc("AppendMenuW")
	pTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	pDestroyMenu       = user32.NewProc("DestroyMenu")
	pClientToScreen    = user32.NewProc("ClientToScreen")
	pOpenClipboard     = user32.NewProc("OpenClipboard")
	pEmptyClipboard    = user32.NewProc("EmptyClipboard")
	pSetClipboardData  = user32.NewProc("SetClipboardData")
	pCloseClipboard    = user32.NewProc("CloseClipboard")
	pGlobalAllocMem    = kernel32.NewProc("GlobalAlloc")
	pGlobalLockMem     = kernel32.NewProc("GlobalLock")
	pGlobalUnlockMem   = kernel32.NewProc("GlobalUnlock")
	pCreateSolidBrush  = gdi32.NewProc("CreateSolidBrush")
	pCreatePen         = gdi32.NewProc("CreatePen")
	pCreateFontW       = gdi32.NewProc("CreateFontW")
	pSelectObject      = gdi32.NewProc("SelectObject")
	pDeleteObject      = gdi32.NewProc("DeleteObject")
	pSetTextColor      = gdi32.NewProc("SetTextColor")
	pSetBkMode         = gdi32.NewProc("SetBkMode")
	pDrawTextW         = user32.NewProc("DrawTextW") // user32, not gdi32
	pFillRect          = user32.NewProc("FillRect")
	pFrameRect         = user32.NewProc("FrameRect")
	pCreateCompatibleD = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatBmp   = gdi32.NewProc("CreateCompatibleBitmap")
	pBitBlt            = gdi32.NewProc("BitBlt")
	pDeleteDC          = gdi32.NewProc("DeleteDC")

	pGetDiskFreeEx     = kernel32.NewProc("GetDiskFreeSpaceExW")
	pGetLogicalDrives  = kernel32.NewProc("GetLogicalDrives")
	pGetConsoleWindow  = kernel32.NewProc("GetConsoleWindow")
	pGetDriveTypeW    = kernel32.NewProc("GetDriveTypeW")
	pGetVolumeInfoW   = kernel32.NewProc("GetVolumeInformationW")

	pShellExecuteW = shell32.NewProc("ShellExecuteW")
	pSHBrowseFor   = shell32.NewProc("SHBrowseForFolderW")
	pSHGetPathFrom = shell32.NewProc("SHGetPathFromIDListW")
	pCoInitialize  = ole32.NewProc("CoInitialize")
	pCoTaskMemFree = ole32.NewProc("CoTaskMemFree")
)

const (
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	cwUseDefault       = 0x80000000
	idcArrow           = 32512
	idiApplication     = 32512
	swShow             = 5
	swShowNormal       = 1
	wmDestroy          = 0x0002
	wmSize             = 0x0005
	wmPaint            = 0x000F
	wmTimer            = 0x0113
	wmKeyDown          = 0x0100
	wmMouseMove        = 0x0200
	wmLButtonDown      = 0x0201
	wmLButtonDblClk    = 0x0203
	wmRButtonDown      = 0x0204
	wmEraseBkgnd       = 0x0014
	wmAppDone          = 0x8001
	dtLeft             = 0x0000
	dtCenter           = 0x0001
	dtRight            = 0x0002
	dtVCenter          = 0x0004
	dtSingleLine       = 0x0020
	dtEndEllipsis      = 0x8000
	dtNoPrefix         = 0x0800
	transparent        = 1
	srcCopy            = 0x00CC0020
)

type rect struct{ Left, Top, Right, Bottom int32 }

type point struct{ X, Y int32 }

type msgStruct struct {
	HWnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type paintStruct struct {
	Hdc         syscall.Handle
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type browseInfoW struct {
	HwndOwner      syscall.Handle
	PidlRoot       uintptr
	PszDisplayName *uint16
	LpszTitle      *uint16
	UlFlags        uint32
	Lpfn           uintptr
	LParam         uintptr
	IImage         int32
}

func u16(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		p, _ = syscall.UTF16PtrFromString("?")
	}
	return p
}

func rgb(r, g, b int32) uint32 { return uint32(r) | uint32(g)<<8 | uint32(b)<<16 }

var (
	colBg      = rgb(7, 10, 18)
	colPanel   = rgb(14, 21, 38)
	colLine    = rgb(27, 39, 65)
	colText    = rgb(230, 237, 247)
	colDim     = rgb(125, 139, 166)
	colDim2    = rgb(84, 97, 122)
	colAccent  = rgb(110, 231, 255)
	colTileDir = rgb(60, 72, 120)
)

// category palette (mirrors ui.html)
var catColor = map[string]uint32{
	"video":   rgb(255, 93, 143),
	"image":   rgb(56, 189, 248),
	"audio":   rgb(167, 139, 250),
	"archive": rgb(251, 191, 36),
	"code":    rgb(52, 211, 153),
	"docs":    rgb(251, 146, 60),
	"system":  rgb(148, 163, 184),
	"other":   rgb(100, 116, 139),
	"free":    rgb(74, 88, 112),
}

// ---------- app state ----------

type hitKind int

const (
	hitDrive hitKind = iota
	hitPick
	hitRescan
	hitUp
	hitTile
	hitBack
	hitForward
)

type hitBox struct {
	r    rect
	kind hitKind
	arg  string
	idx  int
}

type tileBox struct {
	r       rect
	n       *Node
	idx     int
	depth   int
	isDir   bool
	special bool // synthetic (free space), not clickable
}

type appState struct {
	hwnd     syscall.Handle
	root     string
	tree     *Node
	view     []*Node
	scanning bool
	sc       *Scanner
	hits     []hitBox
	tiles    []tileBox
	hover    int
	sel      *Node
	status     string
	totalFiles int64
	freeSpace  int64
	hist       [][]*Node // previous views, newest last
	fwd        [][]*Node // views undone by back
	drives   []driveInfo

	fontUI   uintptr
	fontBig  uintptr
	fontSmall uintptr
	fontMono uintptr
	brushes  map[uint32]uintptr
}

type driveInfo struct {
	path  string // "C:\"
	label string
	kind  string
}

var (
	st         = &appState{hover: -1, brushes: map[uint32]uintptr{}}
	pendingMu  sync.Mutex
	pendingRes *ScanResult
	wndProcPtr uintptr
)

const (
	tbH      = 52
	crumbH   = 26
	statusH  = 26
	startPad = 12
)

func brush(c uint32) uintptr {
	if b, ok := st.brushes[c]; ok {
		return b
	}
	b, _, _ := pCreateSolidBrush.Call(uintptr(c))
	st.brushes[c] = b
	return b
}

func fillRect(hdc uintptr, r rect, c uint32) {
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), brush(c))
}

func frameRect(hdc uintptr, r rect, c uint32) {
	pFrameRect.Call(hdc, uintptr(unsafe.Pointer(&r)), brush(c))
}

func drawText(hdc uintptr, s string, r rect, c uint32, font uintptr, flags uint32) {
	pSetBkMode.Call(hdc, transparent)
	pSetTextColor.Call(hdc, uintptr(c))
	pSelectObject.Call(hdc, font)
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(s))), ^uintptr(0), uintptr(unsafe.Pointer(&r)), uintptr(flags|dtNoPrefix))
}

func newFont(size int32, weight int32, face string) uintptr {
	f, _, _ := pCreateFontW.Call(
		uintptr(-size), 0, 0, 0, uintptr(weight), 0, 0, 0,
		1, 0, 0, 5, 0, uintptr(unsafe.Pointer(u16(face))))
	return f
}

func humanSize(b int64) string { return human(b) }

func shortName(n string, max int) string {
	r := []rune(n)
	if len(r) <= max {
		return n
	}
	return string(r[:max-1]) + "…"
}

// ---------- drives ----------

func listDrives() []driveInfo {
	mask, _, _ := pGetLogicalDrives.Call()
	dbg(fmt.Sprintf("drives mask=%#x", mask))
	var out []driveInfo
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + ":\\"
		t, _, _ := pGetDriveTypeW.Call(uintptr(unsafe.Pointer(u16(root))))
		var kind string
		switch t {
		case 2:
			kind = "removable"
		case 3:
			kind = "local"
		case 4:
			kind = "network"
		case 5:
			kind = "cd"
		case 6:
			kind = "ram"
		default:
			continue
		}
		// NOTE: do NOT call GetVolumeInformationW here. On a mapped network
		// drive (Z:) whose server is offline it blocks for tens of seconds and
		// freezes the UI thread before the window even exists. The toolbar only
		// shows the drive letter, so the call buys nothing anyway.
		out = append(out, driveInfo{path: root, label: "", kind: kind})
	}
	return out
}

// ---------- scanning ----------

func (a *appState) startScan(root string) {
	if a.scanning {
		return
	}
	dbg("startScan " + root)
	root = resolveRoot(root)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		a.status = "เปิดโฟลเดอร์ไม่ได้: " + root
		return
	}
	a.root = root
	// free space of the volume being scanned -> drawn as a tile at drive root
	a.freeSpace = 0
	if vol := filepath.VolumeName(root); vol != "" {
		var avail, total, totalFree uint64
		pGetDiskFreeEx.Call(uintptr(unsafe.Pointer(u16(vol + `\\`))),
			uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)))
		a.freeSpace = int64(avail)
	}
	a.scanning = true
	a.tree = nil
	a.view = nil
	a.hover = -1
	a.sel = nil
	a.status = "กำลังสแกน " + root
	// fewer workers than the CLI: leave CPUs free for the UI thread, otherwise
	// the Win32 message loop starves and Windows paints "(Not Responding)".
	w := runtime.NumCPU() * 2
	if w < 8 {
		w = 8
	}
	a.sc = NewScanner(w)
	go func(sc *Scanner, r string) {
		res := sc.Run(r)
		pendingMu.Lock()
		pendingRes = &res
		pendingMu.Unlock()
		pPostMessageW.Call(uintptr(a.hwnd), wmAppDone, 0, 0)
	}(a.sc, root)
	pSetTimer.Call(uintptr(a.hwnd), 1, 600, 0)
	pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

func (a *appState) finishScan() {
	dbg("finishScan enter")
	pendingMu.Lock()
	res := pendingRes
	pendingRes = nil
	pendingMu.Unlock()
	if res == nil {
		return
	}
	pKillTimer.Call(uintptr(a.hwnd), 1)
	a.scanning = false
	a.status = ""
	a.tree = res.Tree
	a.totalFiles = res.Stats.Files
	domPass(a.tree)
	// GUI needs a deep tree (we don't serialise JSON), so keep far more nodes:
	// a small budget made big folders lose their children and the map looked flat.
	prune(a.tree, intp(900000), 80, 0)
	a.view = []*Node{a.tree}
	a.hist = a.hist[:0]
	a.fwd = a.fwd[:0]
	a.hover = -1
	dbg(fmt.Sprintf("finishScan done files=%d bytes=%d", countFiles(a.tree), a.tree.Size))
	pSetWindowTextW.Call(uintptr(a.hwnd), uintptr(unsafe.Pointer(u16(
		fmt.Sprintf("DEEPFIELD — %s · %s", a.root, humanSize(res.Stats.Bytes))))))
	pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

func (a *appState) cur() *Node {
	if len(a.view) == 0 {
		return nil
	}
	return a.view[len(a.view)-1]
}

func (a *appState) fullPath(n *Node) string {
	parts := make([]string, 0, len(a.view))
	for i, v := range a.view {
		if i == 0 {
			continue
		}
		parts = append(parts, v.Name)
	}
	p := strings.TrimRight(a.root, `\/`)
	if n != nil && len(a.view) > 0 && n != a.cur() {
		parts = append(parts, n.Name)
	}
	if len(parts) > 0 {
		p += `\` + strings.Join(parts, `\`)
	}
	return p
}

// ---------- treemap layout ----------

type sqItem struct {
	value float64
	idx   int
}

func squarify(items []sqItem, x, y, w, h float64) []rect {
	out := make([]rect, 0, len(items))
	total := 0.0
	for _, it := range items {
		total += it.value
	}
	if total <= 0 || w <= 1 || h <= 1 {
		return out
	}
	scale := (w * h) / total
	rest := items
	rx, ry, rw, rh := x, y, w, h
	worst := func(row []sqItem, side float64) float64 {
		sum, mx, mn := 0.0, 0.0, 1e18
		for _, it := range row {
			area := it.value * scale
			sum += area
			if area > mx {
				mx = area
			}
			if area < mn {
				mn = area
			}
		}
		if sum <= 0 || mn <= 0 {
			return 1e18
		}
		s2 := sum * sum
		l2 := side * side
		a1 := l2 * mx / s2
		a2 := s2 / (l2 * mn)
		if a1 > a2 {
			return a1
		}
		return a2
	}
	for len(rest) > 0 {
		side := rw
		if rh < rw {
			side = rh
		}
		if side <= 0.5 {
			break
		}
		row := []sqItem{rest[0]}
		i := 1
		cur := worst(row, side)
		for i < len(rest) {
			test := append(append([]sqItem{}, row...), rest[i])
			wv := worst(test, side)
			if wv <= cur {
				row = test
				cur = wv
				i++
			} else {
				break
			}
		}
		rest = rest[i:]
		rowSum := 0.0
		for _, it := range row {
			rowSum += it.value * scale
		}
		if rw >= rh {
			cw := rowSum / rh
			if cw > rw {
				cw = rw
			}
			yy := ry
			for _, it := range row {
				hh := (it.value * scale) / cw
				if yy+hh > ry+rh {
					hh = ry + rh - yy
				}
				out = append(out, rect{int32(rx), int32(yy), int32(rx + cw), int32(yy + hh)})
				yy += hh
			}
			rx += cw
			rw -= cw
		} else {
			ch := rowSum / rw
			if ch > rh {
				ch = rh
			}
			xx := rx
			for _, it := range row {
				ww := (it.value * scale) / ch
				if xx+ww > rx+rw {
					ww = rx + rw - xx
				}
				out = append(out, rect{int32(xx), int32(ry), int32(xx + ww), int32(ry + ch)})
				xx += ww
			}
			ry += ch
			rh -= ch
		}
	}
	return out
}

func blend(c uint32, bg uint32, alpha float64) uint32 {
	mix := func(a, b uint32) uint32 {
		return uint32(float64(a&0xFF)*alpha + float64(b&0xFF)*(1-alpha))
	}
	r := mix(c&0xFF, bg&0xFF)
	g := mix((c>>8)&0xFF, (bg>>8)&0xFF)
	b := mix((c>>16)&0xFF, (bg>>16)&0xFF)
	return r | g<<8 | b<<16
}

func colorOfNode(n *Node) uint32 {
	key := n.Dom
	if !n.IsDir {
		key = categorize(n.Name)
	}
	if key == "" {
		key = "other"
	}
	c, ok := catColor[key]
	if !ok {
		c = catColor["other"]
	}
	return c
}

// ---------- drawing ----------

func (a *appState) drawToolbar(hdc uintptr, W int32) {
	fillRect(hdc, rect{0, 0, W, tbH}, colPanel)
	fillRect(hdc, rect{0, tbH - 1, W, tbH}, colLine)

	// logo dot + wordmark
	fillRect(hdc, rect{startPad, 20, startPad + 12, 32}, colAccent)
	drawText(hdc, "DEEPFIELD", rect{startPad + 20, 10, startPad + 150, 30}, colText, st.fontBig, dtLeft|dtSingleLine|dtVCenter)
	drawText(hdc, "SPACE ANALYZER", rect{startPad + 20, 28, startPad + 150, 44}, colDim2, st.fontSmall, dtLeft|dtSingleLine|dtVCenter)

	a.hits = a.hits[:0]
	x := int32(startPad + 160)
	y := int32(13)

	// back / forward sit left of the drives; dim when there is nothing to go to
	navBtn := func(label string, kind hitKind, enabled bool) {
		w := int32(len([]rune(label))*7 + 20)
		r := rect{x, y, x + w, y + 26}
		bg, tc := rgb(19, 29, 51), colText
		if !enabled {
			bg, tc = rgb(12, 19, 32), colDim2
		}
		fillRect(hdc, r, bg)
		frameRect(hdc, r, colLine)
		drawText(hdc, label, r, tc, st.fontUI, dtCenter|dtSingleLine|dtVCenter)
		if enabled {
			a.hits = append(a.hits, hitBox{r: r, kind: kind})
		}
		x += w + 5
	}
	navBtn("← ย้อนกลับ", hitBack, len(a.hist) > 0)
	navBtn("→", hitForward, len(a.fwd) > 0)
	x += 8

	// drive buttons
	for _, d := range a.drives {
		label := d.path[:2]
		w := int32(44)
		r := rect{x, y, x + w, y + 26}
		active := strings.EqualFold(strings.TrimRight(a.root, `\`), strings.TrimRight(d.path, `\`))
		bg := uint32(colPanel)
		if active {
			bg = rgb(22, 52, 78)
		}
		fillRect(hdc, r, bg)
		frameRect(hdc, r, colLine)
		tc := colText
		if active {
			tc = colAccent
		}
		drawText(hdc, label, r, tc, st.fontUI, dtCenter|dtSingleLine|dtVCenter)
		a.hits = append(a.hits, hitBox{r: r, kind: hitDrive, arg: d.path})
		x += w + 5
	}

	x += 10
	addBtn := func(label string, kind hitKind, arg string) {
		w := int32(len([]rune(label))*7 + 22)
		r := rect{x, y, x + w, y + 26}
		fillRect(hdc, r, rgb(19, 29, 51))
		frameRect(hdc, r, colLine)
		drawText(hdc, label, r, colText, st.fontUI, dtCenter|dtSingleLine|dtVCenter)
		a.hits = append(a.hits, hitBox{r: r, kind: kind, arg: arg})
		x += w + 6
	}
	addBtn("เลือกโฟลเดอร์…", hitPick, "")
	addBtn("สแกนใหม่", hitRescan, "")
	if len(a.view) > 1 {
		addBtn("↑ ขึ้น", hitUp, "")
	}

	// right side totals
	if a.tree != nil {
		info := fmt.Sprintf("%s   ·   %s ไฟล์", humanSize(a.tree.Size), comma(a.totalFiles))
		r := rect{W - 320, 10, W - startPad, 42}
		drawText(hdc, info, r, colAccent, st.fontMono, dtRight|dtSingleLine|dtVCenter)
	}
}

func countFiles(n *Node) int {
	c := 0
	for _, ch := range n.Children {
		if ch.IsDir {
			c += countFiles(ch)
		} else {
			c++
		}
	}
	return c
}

func comma(v int64) string {
	s := fmt.Sprintf("%d", v)
	out := ""
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(ch)
	}
	return out
}

func (a *appState) drawBreadcrumb(hdc uintptr, W int32) {
	y := int32(tbH)
	fillRect(hdc, rect{0, y, W, y + crumbH}, colBg)
	x := int32(startPad)
	drawText(hdc, shortName(a.root, 40), rect{x, y, x + 320, y + crumbH}, colAccent, st.fontUI, dtLeft|dtSingleLine|dtVCenter)
	x += 330
	for i := 1; i < len(a.view); i++ {
		drawText(hdc, "›", rect{x, y, x + 14, y + crumbH}, colDim2, st.fontUI, dtLeft|dtSingleLine|dtVCenter)
		x += 16
		r := rect{x, y, x + 240, y + crumbH}
		drawText(hdc, shortName(a.view[i].Name, 34), r, colText, st.fontUI, dtLeft|dtSingleLine|dtVCenter)
		idx := i
		a.hits = append(a.hits, hitBox{r: r, kind: hitUp, arg: "", idx: idx})
		x += 250
		if x > W-60 {
			break
		}
	}
	fillRect(hdc, rect{0, y + crumbH - 1, W, y + crumbH}, colLine)
}

const maxTileDepth = 8

// freeNode is the synthetic "free space" box shown at the drive root only.
var freeNode *Node

func (a *appState) drawTiles(hdc uintptr, W, H int32) {
	top := int32(tbH + crumbH)
	bottom := H - statusH
	area := rect{0, top, W, bottom}
	fillRect(hdc, area, colBg)
	a.tiles = a.tiles[:0]
	freeNode = nil

	node := a.cur()
	if node == nil {
		msg := "เลือกไดรฟ์หรือโฟลเดอร์ด้านบนเพื่อเริ่มสแกน"
		if a.scanning {
			msg = "กำลังสแกน…"
		}
		drawText(hdc, msg, rect{0, top, W, bottom}, colDim, st.fontBig, dtCenter|dtSingleLine|dtVCenter)
		return
	}
	if len(node.Children) == 0 {
		drawText(hdc, "โฟลเดอร์ว่าง", rect{0, top, W, bottom}, colDim, st.fontBig, dtCenter|dtSingleLine|dtVCenter)
		return
	}
	a.layout(hdc, node, rect{area.Left + 1, area.Top + 1, area.Right - 1, area.Bottom - 1}, 1)

	// hover frame last so nesting cannot paint over it
	if a.hover >= 0 && a.hover < len(a.tiles) {
		r := a.tiles[a.hover].r
		frameRect(hdc, r, rgb(255, 255, 255))
		frameRect(hdc, rect{r.Left + 1, r.Top + 1, r.Right - 1, r.Bottom - 1}, rgb(120, 220, 255))
	}
}

func sortKids(n *Node) []*Node {
	kids := make([]*Node, 0, len(n.Children))
	for _, c := range n.Children {
		if c.Size > 0 {
			kids = append(kids, c)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Size > kids[j].Size })
	if len(kids) > 200 {
		kids = kids[:200]
	}
	return kids
}

// layout draws one level inside area and then recurses into every folder that
// is big enough, so you see the whole tree nested (like SpaceMonger) instead of
// one flat ring of blocks.
func (a *appState) layout(hdc uintptr, parent *Node, area rect, depth int) {
	if depth > maxTileDepth || area.Right-area.Left < 8 || area.Bottom-area.Top < 8 {
		return
	}
	kids := sortKids(parent)
	if depth == 1 && len(a.view) == 1 && a.freeSpace > 0 {
		freeNode = &Node{Name: "พื้นที่ว่าง", Size: a.freeSpace, Dom: "free"}
		kids = append(kids, freeNode)
		sort.Slice(kids, func(i, j int) bool { return kids[i].Size > kids[j].Size })
	}
	if len(kids) == 0 {
		return
	}
	items := make([]sqItem, len(kids))
	for i, k := range kids {
		v := float64(k.Size)
		if v < 1 {
			v = 1
		}
		items[i] = sqItem{value: v, idx: i}
	}
	rects := squarify(items, float64(area.Left), float64(area.Top),
		float64(area.Right-area.Left), float64(area.Bottom-area.Top))

	for i, rr := range rects {
		if i >= len(kids) || len(a.tiles) > 3600 {
			break
		}
		k := kids[i]
		r := rect{rr.Left + 1, rr.Top + 1, rr.Right - 1, rr.Bottom - 1}
		w, h := r.Right-r.Left, r.Bottom-r.Top
		if w < 3 || h < 3 {
			continue
		}
		base := colorOfNode(k)
		fillRect(hdc, r, shadeForDepth(base, depth, i))
		if w > 10 && h > 8 {
			// bevel: light top/left edge + darker bottom/right edge inside the
			// box. Without the shadow the pastel fills look punched into the bg.
			fillRect(hdc, rect{r.Left + 1, r.Top + 1, r.Right - 1, r.Top + 2}, rgb(255, 255, 255))
			fillRect(hdc, rect{r.Left + 1, r.Top + 1, r.Left + 2, r.Bottom - 1}, rgb(255, 255, 255))
			fillRect(hdc, rect{r.Left + 1, r.Bottom - 2, r.Right - 1, r.Bottom - 1}, rgb(163, 173, 190))
			fillRect(hdc, rect{r.Right - 2, r.Top + 1, r.Right - 1, r.Bottom - 1}, rgb(163, 173, 190))
		}

		labelH := int32(0)
		if w >= 50 && h >= 15 {
			tr := rect{r.Left + 4, r.Top + 1, r.Right - 3, r.Top + 15}
			// dark ink: pastel fills are light, so white text would vanish
			drawText(hdc, k.Name, tr, rgb(38, 48, 68), st.fontUI, dtLeft|dtSingleLine|dtVCenter|dtEndEllipsis)
			labelH = 15
			if h >= 32 {
				sr := rect{r.Left + 4, r.Top + 14, r.Right - 3, r.Top + 29}
				txt := humanSize(k.Size)
				if parent.Size > 0 {
					txt += fmt.Sprintf("  %.1f%%", float64(k.Size)*100/float64(parent.Size))
				}
				drawText(hdc, txt, sr, rgb(92, 104, 126), st.fontMono, dtLeft|dtSingleLine|dtVCenter|dtEndEllipsis)
				labelH = 29
			}
		}
		frameRect(hdc, r, rgb(108, 120, 140))
		if k.IsDir && w > 12 && h > 12 {
			// coloured spine on the left edge marks a folder
			fillRect(hdc, rect{r.Left, r.Top, r.Left + 3, r.Bottom}, blend(base, rgb(255, 255, 255), 0.30))
		}
		a.tiles = append(a.tiles, tileBox{r: r, n: k, idx: len(a.tiles), depth: depth, isDir: k.IsDir, special: k == freeNode})

		if k.IsDir && len(k.Children) > 0 && depth < maxTileDepth {
			inner := rect{r.Left + 3, r.Top + labelH + 1, r.Right - 2, r.Bottom - 2}
			if inner.Right-inner.Left >= 22 && inner.Bottom-inner.Top >= 20 {
				a.layout(hdc, k, inner, depth+1)
			}
		}
	}
}

// shadeForDepth returns a PASTEL tint: the category hue is washed toward white
// (heavy at the top level, a little stronger deeper in) so the map reads soft
// instead of neon, while neighbours still differ.
func shadeForDepth(base uint32, depth, idx int) uint32 {
	r := float64(base & 0xFF)
	g := float64((base >> 8) & 0xFF)
	b := float64((base >> 16) & 0xFF)
	switch idx % 3 {
	case 1:
		r, g = g*0.85+30, r*0.85+22
	case 2:
		g, b = b*0.85+30, g*0.85+22
	}
	lift := 0.62 - 0.055*float64(depth-1)
	if lift < 0.22 {
		lift = 0.22
	}
	wr, wg, wb := 247.0, 247.0, 252.0 // near-white target
	r = r*(1-lift) + wr*lift
	g = g*(1-lift) + wg*lift
	b = b*(1-lift) + wb*lift
	cl := func(v float64) uint32 {
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		return uint32(v)
	}
	return cl(r) | cl(g)<<8 | cl(b)<<16
}

func (a *appState) drawStatus(hdc uintptr, W, H int32) {
	y := H - statusH
	fillRect(hdc, rect{0, y, W, H}, colPanel)
	fillRect(hdc, rect{0, y, W, y + 1}, colLine)
	txt := ""
	c := colDim
	if a.hover >= 0 && a.hover < len(a.tiles) {
		t := a.tiles[a.hover]
		cur := a.cur()
		pct := 0.0
		if cur != nil && cur.Size > 0 {
			pct = float64(t.n.Size) * 100 / float64(cur.Size)
		}
		kind := "ไฟล์"
		if t.isDir {
			kind = "โฟลเดอร์"
		}
		txt = fmt.Sprintf("%s  ·  %s  ·  %s  ·  %.1f%% ของโฟลเดอร์นี้  ·  %s",
			t.n.Name, kind, humanSize(t.n.Size), pct, a.fullPath(t.n))
		c = colText
	} else if a.scanning && a.sc != nil {
		files := loadInt64(&a.sc.files)
		by := loadInt64(&a.sc.bytes)
		txt = fmt.Sprintf("กำลังสแกน…  %s ไฟล์  ·  %s  ·  worker %d",
			comma(files), humanSize(by), cap(a.sc.sem))
		c = colAccent
	} else if a.status != "" {
		txt = a.status
	} else if a.tree != nil {
		txt = fmt.Sprintf("%s  ·  %s  ·  %s ไฟล์  ·  คลิกซูม  ·  ← ย้อนกลับ  ·  คลิกขวา = เปิดโฟลเดอร์จริงใน Explorer",
			a.root, humanSize(a.tree.Size), comma(a.totalFiles))
	}
	drawText(hdc, txt, rect{startPad, y, W - startPad, H}, c, st.fontSmall, dtLeft|dtSingleLine|dtVCenter|dtEndEllipsis)
}

func (a *appState) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	var rc rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	W, H := rc.Right, rc.Bottom

	mdc, _, _ := pCreateCompatibleD.Call(hdc)
	bmp, _, _ := pCreateCompatBmp.Call(hdc, uintptr(W), uintptr(H))
	old, _, _ := pSelectObject.Call(mdc, bmp)

	fillRect(mdc, rect{0, 0, W, H}, colBg)
	a.drawToolbar(mdc, W)
	a.drawBreadcrumb(mdc, W)
	a.drawTiles(mdc, W, H)
	a.drawStatus(mdc, W, H)

	pBitBlt.Call(hdc, 0, 0, uintptr(W), uintptr(H), mdc, 0, 0, srcCopy)
	pSelectObject.Call(mdc, old)
	pDeleteObject.Call(bmp)
	pDeleteDC.Call(mdc)
	pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

// ---------- input ----------

// navigate switches the visible folder and remembers where we came from.
// Everything is already scanned into memory, so this never re-reads the disk.
func (a *appState) navigate(next []*Node) {
	a.hist = append(a.hist, append([]*Node{}, a.view...))
	a.fwd = a.fwd[:0]
	a.view = append([]*Node{}, next...)
	a.hover = -1
	pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

func (a *appState) goBack() {
	if len(a.hist) == 0 {
		return
	}
	prev := a.hist[len(a.hist)-1]
	a.hist = a.hist[:len(a.hist)-1]
	a.fwd = append(a.fwd, append([]*Node{}, a.view...))
	a.view = prev
	a.hover = -1
	pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

func (a *appState) goForward() {
	if len(a.fwd) == 0 {
		return
	}
	next := a.fwd[len(a.fwd)-1]
	a.fwd = a.fwd[:len(a.fwd)-1]
	a.hist = append(a.hist, append([]*Node{}, a.view...))
	a.view = next
	a.hover = -1
	pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

// hitTile returns the innermost tile under the cursor (deepest box wins).
func (a *appState) hitTile(x, y int32) *tileBox {
	for i := len(a.tiles) - 1; i >= 0; i-- {
		t := &a.tiles[i]
		if x >= t.r.Left && x <= t.r.Right && y >= t.r.Top && y <= t.r.Bottom {
			return t
		}
	}
	return nil
}

func (a *appState) onClick(x, y int32) {
	dbg(fmt.Sprintf("click %d,%d hits=%d tiles=%d", x, y, len(a.hits), len(a.tiles)))
	for _, h := range a.hits {
		if x < h.r.Left || x > h.r.Right || y < h.r.Top || y > h.r.Bottom {
			continue
		}
		switch h.kind {
		case hitDrive:
			a.startScan(h.arg)
		case hitPick:
			a.startScan(pickFolder(a.hwnd))
		case hitRescan:
			if a.root != "" {
				a.startScan(a.root)
			}
		case hitBack:
			a.goBack()
		case hitForward:
			a.goForward()
		case hitUp:
			if h.idx > 0 && h.idx < len(a.view) {
				a.navigate(a.view[:h.idx])
			}
		}
		return
	}
	if t := a.hitTile(x, y); t != nil {
		if t.special {
			return
		}
		if t.isDir && len(t.n.Children) > 0 {
			a.navigate(append(append([]*Node{}, a.view...), t.n))
		} else {
			a.sel = t.n
			a.status = fmt.Sprintf("%s  ·  %s  ·  %s", t.n.Name, humanSize(t.n.Size), a.fullPath(t.n))
			pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
		}
	}
}

func (a *appState) onDoubleClick(x, y int32) {
	if t := a.hitTile(x, y); t != nil && !t.special {
		revealInExplorer(a.fullPath(t.n))
	}
}

// openFolderInExplorer opens Explorer *inside* the folder (not just selects
// it), which is what right-click on a tile should do.
func openFolderInExplorer(path string) {
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))),
		uintptr(unsafe.Pointer(u16("explorer.exe"))),
		uintptr(unsafe.Pointer(u16(`"`+path+`"`))), 0, swShowNormal)
}

// copyText puts a string on the Windows clipboard (CF_UNICODETEXT).
func copyText(s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	h, _, _ := pGlobalAllocMem.Call(0x0042, uintptr(len(u)*2)) // GMEM_MOVEABLE|ZEROINIT
	if h == 0 {
		return
	}
	if p, _, _ := pGlobalLockMem.Call(h); p != 0 {
		dst := (*[1 << 16]uint16)(unsafe.Pointer(p))[:len(u):len(u)]
		copy(dst, u)
		pGlobalUnlockMem.Call(h)
	}
	pOpenClipboard.Call(uintptr(st.hwnd))
	pEmptyClipboard.Call()
	pSetClipboardData.Call(13, h) // CF_UNICODETEXT
	pCloseClipboard.Call()
}

// onRightClick pops a small context menu instead of acting immediately.
func (a *appState) onRightClick(x, y int32) {
	t := a.hitTile(x, y)
	if t == nil || t.special {
		return
	}
	path := a.fullPath(t.n)

	hMenu, _, _ := pCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	pAppendMenuW.Call(hMenu, 0, 1, uintptr(unsafe.Pointer(u16("เปิด Explorer"))))
	pAppendMenuW.Call(hMenu, 0, 2, uintptr(unsafe.Pointer(u16("คัดลอกพาธ"))))
	canZoom := t.isDir && len(t.n.Children) > 0
	if canZoom {
		pAppendMenuW.Call(hMenu, 0, 3, uintptr(unsafe.Pointer(u16("ซูมเข้าที่นี่"))))
	}
	pt := point{X: x, Y: y}
	pClientToScreen.Call(uintptr(a.hwnd), uintptr(unsafe.Pointer(&pt)))
	cmd, _, _ := pTrackPopupMenu.Call(hMenu, 0x0100, // TPM_RETURNCMD
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(a.hwnd), 0)
	pDestroyMenu.Call(hMenu)

	switch cmd {
	case 1:
		if t.isDir {
			openFolderInExplorer(path)
		} else {
			revealInExplorer(path)
		}
		a.status = "เปิดใน Explorer: " + path
	case 2:
		copyText(path)
		a.status = "คัดลอกพาธแล้ว: " + path
	case 3:
		if canZoom {
			a.navigate(append(append([]*Node{}, a.view...), t.n))
			return
		}
	}
	pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

func revealInExplorer(path string) {
	arg := "/select,\"" + path + "\""
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))),
		uintptr(unsafe.Pointer(u16("explorer.exe"))),
		uintptr(unsafe.Pointer(u16(arg))), 0, swShowNormal)
}

func (a *appState) onMouseMove(x, y int32) {
	idx := -1
	if t := a.hitTile(x, y); t != nil {
		idx = t.idx
	}
	if idx != a.hover {
		a.hover = idx
		pInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
	}
}

func pickFolder(owner syscall.Handle) string {
	var buf [261]uint16
	bi := browseInfoW{
		HwndOwner:      owner,
		PszDisplayName: &buf[0],
		LpszTitle:      u16("เลือกโฟลเดอร์หรือไดรฟ์ที่ต้องการสแกน"),
		UlFlags:        0x0001 | 0x0010 | 0x0040, // RETURNONLYFSDIRS | EDITBOX | NEWDIALOGSTYLE
	}
	pidl, _, _ := pSHBrowseFor.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	var path [261]uint16
	ok, _, _ := pSHGetPathFrom.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	pCoTaskMemFree.Call(pidl)
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(path[:])
}

// ---------- window ----------

const className = "DeepFieldMainWindow"

func loadInt64(p *int64) int64 {
	// atomic read without importing sync/atomic here (same package already has it
	// via main.go, but keep this file self-contained)
	return *(*int64)(unsafe.Pointer(p))
}

func wndProc(hwnd syscall.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		st.paint(uintptr(hwnd))
		return 0
	case wmAppDone:
		st.finishScan()
		return 0
	case wmTimer:
		if st.scanning {
			// repaint only the status strip: a full-window repaint every tick
			// makes the message queue back up while a big scan is running
			var rc rect
			pGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
			sr := rect{0, rc.Bottom - statusH, rc.Right, rc.Bottom}
			pInvalidateRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&sr)), 0)
		}
		return 0
	case wmMouseMove:
		st.onMouseMove(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
		return 0
	case wmLButtonDown:
		st.onClick(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
		return 0
	case wmLButtonDblClk:
		st.onDoubleClick(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
		return 0
	case wmRButtonDown:
		st.onRightClick(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
		return 0
	case wmKeyDown:
		switch wparam {
		case 0x08: // backspace
			st.goBack()
		case 0x25: // left arrow
			st.goBack()
		case 0x27: // right arrow
			st.goForward()
		case 0x74: // F5
			if st.root != "" {
				st.startScan(st.root)
			}
		}
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return r
}

func dbg(msg string) {
	if os.Getenv("DEEPFIELD_DEBUG") == "" {
		return // opt-in tracing only: set DEEPFIELD_DEBUG=1
	}
	f, err := os.OpenFile(os.TempDir()+`\deepfield-gui.log`, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		fmt.Fprintf(f, "%s\n", msg)
		f.Close()
	}
}

// runGUI opens the native window. root == "" means: start on the drive the exe
// lives on and scan it immediately.
func runGUI(root string, workers int) {
	runtime.LockOSThread() // the message loop must stay on this thread
	dbg("runGUI start")
	pCoInitialize.Call(0)
	// the binary keeps the console subsystem so `deepfield top/scan/version`
	// still print; in GUI mode hide that console so it looks like a real app.
	if cw, _, _ := pGetConsoleWindow.Call(); cw != 0 {
		pShowWindow.Call(cw, 0)
	}
	st.drives = listDrives()

	if root == "" {
		exe, err := os.Executable()
		vol := "C:\\"
		if err == nil {
			if v := filepath.VolumeName(exe); v != "" {
				vol = v + "\\"
			}
		}
		root = vol
	}
	root = resolveRoot(root)

	hInst, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	wndProcPtr = syscall.NewCallback(wndProc)

	cursor, _, _ := pLoadCursorW.Call(0, idcArrow)
	icon, _, _ := pLoadIconW.Call(0, idiApplication)

	cls := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		Style:         0x0003 | 0x0008, // CS_HREDRAW|CS_VREDRAW|CS_DBLCLKS
		LpfnWndProc:   wndProcPtr,
		HInstance:     syscall.Handle(hInst),
		HIcon:         syscall.Handle(icon),
		HCursor:       syscall.Handle(cursor),
		HbrBackground: 0,
		LpszClassName: u16(className),
		HIconSm:       syscall.Handle(icon),
	}
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls))); r == 0 {
		dbg(fmt.Sprintf("RegisterClassEx failed: %v", err))
		fmt.Fprintf(os.Stderr, "RegisterClassEx failed: %v\n", err)
		return
	}
	dbg("class registered")

	sw, _, _ := pGetSystemMetrics.Call(0) // SM_CXSCREEN
	sh, _, _ := pGetSystemMetrics.Call(1)
	w := int32(sw) * 82 / 100
	h := int32(sh) * 80 / 100
	x := (int32(sw) - w) / 2
	y := (int32(sh) - h) / 2

	hwnd, _, err := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(className))),
		uintptr(unsafe.Pointer(u16("DEEPFIELD — space analyzer"))),
		wsOverlappedWindow|wsVisible,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, uintptr(hInst), 0)
	if hwnd == 0 {
		fmt.Fprintf(os.Stderr, "CreateWindowEx failed: %v\n", err)
		return
	}
	st.hwnd = syscall.Handle(hwnd)
	st.fontUI = newFont(14, 400, "Leelawadee UI")
	st.fontBig = newFont(17, 700, "Leelawadee UI")
	st.fontSmall = newFont(12, 400, "Leelawadee UI")
	st.fontMono = newFont(13, 400, "Consolas")

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	pSetForegroundWin.Call(hwnd)

	// auto-scan on start so the window is never a blank slate
	st.startScan(root)
	_ = workers

	var m msgStruct
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
