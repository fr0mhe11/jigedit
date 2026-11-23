package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// --- [ TUI 파일 피커 (Dolphin 스타일 2패널) & 확인 대화상자 ] ---

type FilePickerMode int

const (
	fpModeOpen FilePickerMode = iota
	fpModeSaveAs
)

type FilePickerFocus int

const (
	fpFocusList FilePickerFocus = iota
	fpFocusPlaces
	fpFocusFilter
	fpFocusFilename
	fpFocusPath
	fpFocusNewDir
)

type fpSortCol int

const (
	fpSortName fpSortCol = iota
	fpSortSize
	fpSortModTime
)

type FileEntry struct {
	Name         string
	Path         string
	IsDir        bool
	IsParent     bool
	IsHidden     bool
	IsSymlink    bool
	IsBrokenLink bool
	Size         int64
	ModTime      time.Time
}

type fpPlace struct {
	LabelID  string
	RawLabel string
	Path     string
}

func (p fpPlace) displayLabel() string {
	if p.LabelID != "" {
		return T(p.LabelID)
	}
	return p.RawLabel
}

type fpRect struct {
	X1, X2, Y int
}

func (r fpRect) hit(mx, my int) bool {
	return r.X2 >= r.X1 && my == r.Y && mx >= r.X1 && mx <= r.X2
}

type fpCrumb struct {
	Label string
	Path  string
	Rect  fpRect
}

// fpTextInput 은 파일 피커 내부의 단일 행 텍스트 입력(경로/필터/파일명/새 폴더)을 담당한다.
type fpTextInput struct {
	runes   []rune
	cx      int
	hOffset int
}

func (ti *fpTextInput) text() string {
	return string(ti.runes)
}

func (ti *fpTextInput) set(s string) {
	ti.runes = []rune(s)
	ti.cx = len(ti.runes)
	ti.hOffset = 0
}

func (ti *fpTextInput) clear() {
	ti.runes = nil
	ti.cx = 0
	ti.hOffset = 0
}

func (ti *fpTextInput) clampCursor() {
	if ti.cx < 0 {
		ti.cx = 0
	}
	if ti.cx > len(ti.runes) {
		ti.cx = len(ti.runes)
	}
}

func (ti *fpTextInput) insertRune(r rune) {
	ti.clampCursor()
	ti.runes = append(ti.runes, 0)
	copy(ti.runes[ti.cx+1:], ti.runes[ti.cx:])
	ti.runes[ti.cx] = r
	ti.cx++
}

func (ti *fpTextInput) insertString(s string) {
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsPrint(r) {
			ti.insertRune(r)
		}
	}
}

func (ti *fpTextInput) handleKey(ev *tcell.EventKey) bool {
	ti.clampCursor()
	switch ev.Key() {
	case tcell.KeyLeft:
		if ti.cx > 0 {
			ti.cx--
		}
		return true
	case tcell.KeyRight:
		if ti.cx < len(ti.runes) {
			ti.cx++
		}
		return true
	case tcell.KeyHome, tcell.KeyCtrlA:
		ti.cx = 0
		return true
	case tcell.KeyEnd, tcell.KeyCtrlE:
		ti.cx = len(ti.runes)
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if ti.cx > 0 {
			copy(ti.runes[ti.cx-1:], ti.runes[ti.cx:])
			ti.runes = ti.runes[:len(ti.runes)-1]
			ti.cx--
		}
		return true
	case tcell.KeyDelete:
		if ti.cx < len(ti.runes) {
			copy(ti.runes[ti.cx:], ti.runes[ti.cx+1:])
			ti.runes = ti.runes[:len(ti.runes)-1]
		}
		return true
	case tcell.KeyCtrlU:
		ti.clear()
		return true
	case tcell.KeyCtrlV:
		if clip := firstNonEmptyLine(globalClipboard.ReadSync()); clip != "" {
			ti.insertString(clip)
		}
		return true
	case tcell.KeyRune:
		r := ev.Rune()
		if unicode.IsPrint(r) {
			ti.insertRune(r)
			return true
		}
	}
	return false
}

func (ti *fpTextInput) clickAt(mx, startX, boxW int) {
	if boxW <= 0 {
		return
	}
	ti.clampCursor()
	leftArrow := 0
	if ti.hOffset > 0 {
		leftArrow = 1
	}
	relX := mx - startX - leftArrow
	if relX <= 0 {
		w := 0
		for i, r := range ti.runes {
			if w >= ti.hOffset {
				ti.cx = i
				return
			}
			w += runewidth.RuneWidth(r)
		}
		ti.cx = 0
		return
	}
	targetW := ti.hOffset + relX
	w := 0
	for i, r := range ti.runes {
		rw := runewidth.RuneWidth(r)
		if w+rw > targetW {
			ti.cx = i
			return
		}
		w += rw
	}
	ti.cx = len(ti.runes)
}

func (ti *fpTextInput) draw(setCell func(int, int, rune, []rune, tcell.Style), x, y, maxW int, style, arrowStyle tcell.Style, focused bool) int {
	if maxW <= 0 {
		return -1
	}
	ti.clampCursor()
	for i := 0; i < maxW; i++ {
		setCell(x+i, y, ' ', nil, style)
	}

	totalW := runewidth.StringWidth(string(ti.runes))
	cW := runewidth.StringWidth(string(ti.runes[:ti.cx]))

	if !focused {
		ti.hOffset = 0
		if totalW > maxW {
			curX := x
			limit := x + maxW - 1
			for _, r := range ti.runes {
				rw := runewidth.RuneWidth(r)
				if curX+rw > limit {
					break
				}
				setCell(curX, y, r, nil, style)
				curX += rw
			}
			setCell(x+maxW-1, y, '>', nil, arrowStyle)
		} else {
			curX := x
			for _, r := range ti.runes {
				rw := runewidth.RuneWidth(r)
				if curX+rw > x+maxW {
					break
				}
				setCell(curX, y, r, nil, style)
				curX += rw
			}
		}
		return -1
	}

	if maxW <= 2 {
		return x
	}
	for iter := 0; iter < 4; iter++ {
		leftArrow := 0
		if ti.hOffset > 0 {
			leftArrow = 1
		}
		rightArrow := 0
		if totalW-ti.hOffset > maxW-leftArrow {
			rightArrow = 1
		}
		avail := maxW - 1 - leftArrow - rightArrow
		if avail < 1 {
			avail = 1
		}
		if cW < ti.hOffset {
			ti.hOffset = cW
			continue
		}
		if cW-ti.hOffset > avail {
			ti.hOffset = cW - avail
			if ti.hOffset < 0 {
				ti.hOffset = 0
			}
			continue
		}
		break
	}

	leftArrow := 0
	if ti.hOffset > 0 {
		leftArrow = 1
	}
	rightArrow := totalW-ti.hOffset > maxW-leftArrow

	curX := x
	if leftArrow == 1 {
		setCell(curX, y, '<', nil, arrowStyle)
		curX++
	}
	limitX := x + maxW
	if rightArrow {
		limitX = x + maxW - 1
	}

	strW := 0
	for _, r := range ti.runes {
		rw := runewidth.RuneWidth(r)
		if strW >= ti.hOffset {
			if curX+rw > limitX {
				break
			}
			setCell(curX, y, r, nil, style)
			curX += rw
		}
		strW += rw
	}
	if rightArrow {
		setCell(x+maxW-1, y, '>', nil, arrowStyle)
	}

	cursorX := x + leftArrow + (cW - ti.hOffset)
	if cursorX < x {
		cursorX = x
	}
	if cursorX >= x+maxW {
		cursorX = x + maxW - 1
	}
	return cursorX
}

// FilePicker 는 Dolphin 스타일 2패널(좌측 Places + 우측 상세 테이블) TUI 파일 선택기다.
type FilePicker struct {
	mode       FilePickerMode
	readOnly   bool
	currentDir string
	allEntries []FileEntry
	entries    []FileEntry
	cursor     int
	scrollOff  int
	showHidden bool
	focus      FilePickerFocus
	prevFocus  FilePickerFocus

	// Dolphin 정렬 기준 (폴더는 항상 상단 고정)
	sortCol fpSortCol
	sortAsc bool

	// Dolphin 탐색 히스토리 (뒤로/앞으로)
	history    []string
	historyIdx int

	// 좌측 위치(Places) 사이드바
	places          []fpPlace
	placeCursor     int
	placesScrollOff int

	// 하단 상태 요약 캐시
	dirCount       int
	fileCount      int
	totalFileBytes int64

	// 마우스 호버 / 스크롤바 드래그 상태
	hoverListIdx      int
	hoverPlaceIdx     int
	draggingScrollbar bool

	filterInput   fpTextInput
	filenameInput fpTextInput
	pathInput     fpTextInput
	newDirInput   fpTextInput

	errorMsg string

	onSelect func(path string)
	onCancel func()

	// 렌더링 후 갱신되는 마우스 히트박스
	boxX, boxY, boxW, boxH int
	showPlaces             bool
	placesX1, placesX2     int
	listX1, listX2         int
	listY1, listY2         int
	scrollbarX             int

	backBtn        fpRect
	fwdBtn         fpRect
	upBtn          fpRect
	pathBox        fpRect
	crumbs         []fpCrumb
	newDirBtn      fpRect
	hiddenChk      fpRect
	colNameHdr     fpRect
	colSizeHdr     fpRect
	colDateHdr     fpRect
	scrollUpBtn    fpRect
	scrollDownBtn  fpRect
	filterBox      fpRect
	bottomInputBox fpRect
	okBtn          fpRect
	cancelBtn      fpRect

	cursorX int
	cursorY int
}

// ConfirmDialog 는 대용량 파일 경고 및 덮어쓰기 확인을 위한 TUI 모달 대화상자다.
type ConfirmDialog struct {
	title     string
	message   string
	yesLabel  string
	noLabel   string
	focusYes  bool
	onConfirm func()
	onCancel  func()

	boxX, boxY, boxW, boxH int
	yesBtn                 fpRect
	noBtn                  fpRect
}

func expandHomePath(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func formatFileSize(size int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case size >= gb:
		return fmt.Sprintf("%.1f GB", float64(size)/float64(gb))
	case size >= mb:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(mb))
	case size >= kb:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(kb))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func truncateRightByWidth(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= maxW {
		return s
	}
	if maxW <= 3 {
		var b strings.Builder
		w := 0
		for _, r := range s {
			rw := runewidth.RuneWidth(r)
			if w+rw > maxW {
				break
			}
			b.WriteRune(r)
			w += rw
		}
		return b.String()
	}
	var b strings.Builder
	w := 0
	limit := maxW - 3
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > limit {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteString("...")
	return b.String()
}

// readXDGUserDirs 는 ~/.config/user-dirs.dirs 에서 XDG 기본 폴더 경로를 읽는다.
func readXDGUserDirs(home string) map[string]string {
	out := map[string]string{}
	if home == "" {
		return out
	}
	f, err := os.Open(filepath.Join(home, ".config", "user-dirs.dirs"))
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, "\"'")
		val = strings.ReplaceAll(val, "$HOME", home)
		if val != "" {
			out[key] = filepath.Clean(val)
		}
	}
	return out
}

func buildPlaces(startDir string) []fpPlace {
	var places []fpPlace
	seen := map[string]bool{}

	addPlace := func(labelID, rawLabel, dir string) {
		if dir == "" {
			return
		}
		clean := filepath.Clean(dir)
		if seen[clean] {
			return
		}
		info, err := os.Stat(clean)
		if err != nil || !info.IsDir() {
			return
		}
		seen[clean] = true
		places = append(places, fpPlace{LabelID: labelID, RawLabel: rawLabel, Path: clean})
	}

	home, _ := os.UserHomeDir()
	xdg := readXDGUserDirs(home)
	xdgOr := func(key, fallbackSub string) string {
		if p, ok := xdg[key]; ok && p != "" {
			return p
		}
		if home != "" {
			return filepath.Join(home, fallbackSub)
		}
		return ""
	}

	if home != "" {
		addPlace("fp.places.home", "", home)
	}
	addPlace("fp.places.desktop", "", xdgOr("XDG_DESKTOP_DIR", "Desktop"))
	addPlace("fp.places.documents", "", xdgOr("XDG_DOCUMENTS_DIR", "Documents"))
	addPlace("fp.places.downloads", "", xdgOr("XDG_DOWNLOAD_DIR", "Downloads"))
	addPlace("fp.places.pictures", "", xdgOr("XDG_PICTURES_DIR", "Pictures"))
	addPlace("fp.places.music", "", xdgOr("XDG_MUSIC_DIR", "Music"))
	addPlace("fp.places.videos", "", xdgOr("XDG_VIDEOS_DIR", "Videos"))
	if home != "" {
		addPlace("", ".config", filepath.Join(home, ".config"))
	}
	if startDir != "" {
		base := filepath.Base(filepath.Clean(startDir))
		if base == "" || base == "/" {
			base = startDir
		}
		addPlace("", base, startDir)
	}
	addPlace("fp.places.root", "", string(filepath.Separator))
	if home != "" {
		addPlace("fp.places.trash", "", filepath.Join(home, ".local", "share", "Trash", "files"))
	}
	return places
}

func newFilePicker(mode FilePickerMode, startDir, defaultName string, readOnly bool, onSelect func(string), onCancel func()) *FilePicker {
	fp := &FilePicker{
		mode:          mode,
		readOnly:      readOnly,
		showHidden:    true, // 💡 기본값: 닷파일 및 숨김 폴더 전체 표시
		sortCol:       fpSortName,
		sortAsc:       true,
		historyIdx:    -1,
		hoverListIdx:  -1,
		hoverPlaceIdx: -1,
		scrollbarX:    -1,
		onSelect:      onSelect,
		onCancel:      onCancel,
		cursorX:       -1,
		cursorY:       -1,
	}
	if mode == fpModeSaveAs {
		fp.focus = fpFocusFilename
	} else {
		fp.focus = fpFocusList
	}

	dir := startDir
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = home
		} else {
			dir = string(filepath.Separator)
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		} else if home, err := os.UserHomeDir(); err == nil {
			dir = home
		} else {
			dir = string(filepath.Separator)
		}
	}

	fp.places = buildPlaces(dir)
	if fp.loadDir(dir, "") {
		fp.history = []string{fp.currentDir}
		fp.historyIdx = 0
	}
	if mode == fpModeSaveAs && defaultName != "" {
		fp.filenameInput.set(defaultName)
	}
	return fp
}

func (fp *FilePicker) sortEntries() {
	if len(fp.allEntries) == 0 {
		return
	}
	start := 0
	if fp.allEntries[0].IsParent {
		start = 1
	}
	rest := fp.allEntries[start:]
	sort.Slice(rest, func(i, j int) bool {
		a, b := rest[i], rest[j]
		// Dolphin 기본 동작: 폴더를 항상 파일보다 위에 그룹화
		if a.IsDir != b.IsDir {
			return a.IsDir
		}

		var cmp int
		switch fp.sortCol {
		case fpSortSize:
			if a.Size < b.Size {
				cmp = -1
			} else if a.Size > b.Size {
				cmp = 1
			}
		case fpSortModTime:
			if a.ModTime.Before(b.ModTime) {
				cmp = -1
			} else if a.ModTime.After(b.ModTime) {
				cmp = 1
			}
		}

		if cmp == 0 {
			la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
			if la < lb {
				cmp = -1
			} else if la > lb {
				cmp = 1
			} else if a.Name < b.Name {
				cmp = -1
			} else if a.Name > b.Name {
				cmp = 1
			}
			// 크기/수정일이 동률일 때는 이름 오름차순으로 안정 정렬
			if fp.sortCol != fpSortName {
				return cmp < 0
			}
		}
		if fp.sortAsc {
			return cmp < 0
		}
		return cmp > 0
	})
}

func (fp *FilePicker) toggleSort(col fpSortCol) {
	curName := ""
	if fp.cursor >= 0 && fp.cursor < len(fp.entries) {
		curName = fp.entries[fp.cursor].Name
	}
	if fp.sortCol == col {
		fp.sortAsc = !fp.sortAsc
	} else {
		fp.sortCol = col
		fp.sortAsc = (col == fpSortName)
	}
	fp.sortEntries()
	fp.applyFilter(curName)
}

func (fp *FilePicker) readDirEntries(dir string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	dirEntries, err := os.ReadDir(absDir)
	if err != nil {
		fp.errorMsg = Tf("fp.err.open_dir", err)
		return false
	}

	fp.currentDir = absDir
	fp.errorMsg = ""

	fp.allEntries = make([]FileEntry, 0, len(dirEntries)+1)
	parent := filepath.Dir(absDir)
	if parent != absDir {
		fp.allEntries = append(fp.allEntries, FileEntry{
			Name:     "..",
			Path:     parent,
			IsDir:    true,
			IsParent: true,
		})
	}

	for _, de := range dirEntries {
		name := de.Name()
		fullPath := filepath.Join(absDir, name)
		isHidden := strings.HasPrefix(name, ".")
		isSymlink := de.Type()&os.ModeSymlink != 0
		isDir := de.IsDir()
		isBroken := false
		var size int64
		var modTime time.Time

		if isSymlink {
			if st, err := os.Stat(fullPath); err == nil {
				isDir = st.IsDir()
				size = st.Size()
				modTime = st.ModTime()
			} else {
				isBroken = true
				if info, err := de.Info(); err == nil {
					modTime = info.ModTime()
				}
			}
		} else {
			if info, err := de.Info(); err == nil {
				if !isDir {
					size = info.Size()
				}
				modTime = info.ModTime()
			}
		}

		fp.allEntries = append(fp.allEntries, FileEntry{
			Name:         name,
			Path:         fullPath,
			IsDir:        isDir,
			IsHidden:     isHidden,
			IsSymlink:    isSymlink,
			IsBrokenLink: isBroken,
			Size:         size,
			ModTime:      modTime,
		})
	}
	return true
}

func (fp *FilePicker) loadDir(dir string, selectName string) bool {
	if !fp.readDirEntries(dir) {
		return false
	}
	fp.filterInput.clear()
	fp.hoverListIdx = -1
	fp.sortEntries()
	fp.applyFilter(selectName)
	fp.syncActivePlaceCursor()
	return true
}

func (fp *FilePicker) isFilenameTrackingDirOrEmpty() bool {
	txt := strings.TrimSpace(fp.filenameInput.text())
	return txt == "" || txt == ".." || strings.HasSuffix(txt, "/") || strings.HasSuffix(txt, string(filepath.Separator))
}

// refreshDirKeepState 는 현재 디렉토리를 디스크에서 다시 읽어 필터/스크롤/커서를 최대한 유지하며 갱신한다.
// 외부에서 디렉토리·파일이 생성/삭제되거나 F5 새로고침 시 호출된다.
func (fp *FilePicker) refreshDirKeepState() bool {
	curName := ""
	if fp.cursor >= 0 && fp.cursor < len(fp.entries) {
		curName = fp.entries[fp.cursor].Name
	}
	prevCursor := fp.cursor
	prevScroll := fp.scrollOff

	if !fp.readDirEntries(fp.currentDir) {
		return false
	}
	fp.sortEntries()

	query := strings.ToLower(strings.TrimSpace(fp.filterInput.text()))
	fp.entries = fp.entries[:0]
	fp.dirCount = 0
	fp.fileCount = 0
	fp.totalFileBytes = 0

	for _, ent := range fp.allEntries {
		if ent.IsParent {
			if query == "" {
				fp.entries = append(fp.entries, ent)
			}
			continue
		}
		if !fp.showHidden && ent.IsHidden {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(ent.Name), query) {
			continue
		}
		fp.entries = append(fp.entries, ent)
		if ent.IsDir {
			fp.dirCount++
		} else {
			fp.fileCount++
			fp.totalFileBytes += ent.Size
		}
	}

	found := false
	if curName != "" {
		for i, ent := range fp.entries {
			if ent.Name == curName {
				fp.cursor = i
				found = true
				break
			}
		}
	}
	if !found {
		fp.cursor = prevCursor
		if fp.cursor >= len(fp.entries) {
			fp.cursor = len(fp.entries) - 1
		}
		if fp.cursor < 0 {
			fp.cursor = 0
		}
	}
	fp.scrollOff = prevScroll
	fp.clampScroll()

	newName := ""
	if fp.cursor >= 0 && fp.cursor < len(fp.entries) {
		newName = fp.entries[fp.cursor].Name
	}
	if fp.focus != fpFocusFilename && fp.focus != fpFocusNewDir {
		if fp.mode == fpModeOpen || fp.isFilenameTrackingDirOrEmpty() || newName != curName {
			fp.syncFilenamePreview()
		}
	}
	return true
}

func (fp *FilePicker) syncActivePlaceCursor() {
	cleanCur := filepath.Clean(fp.currentDir)
	for i, pl := range fp.places {
		if pl.Path == cleanCur {
			fp.placeCursor = i
			return
		}
	}
}

func (fp *FilePicker) navigateTo(e *Editor, dir string, selectName string) bool {
	if !fp.loadDir(dir, selectName) {
		return false
	}
	if e != nil {
		e.lastPickerDir = fp.currentDir
		e.watchPickerDir(fp.currentDir)
	}
	if fp.historyIdx < 0 || fp.historyIdx >= len(fp.history) || fp.history[fp.historyIdx] != fp.currentDir {
		if fp.historyIdx+1 < len(fp.history) {
			fp.history = fp.history[:fp.historyIdx+1]
		}
		fp.history = append(fp.history, fp.currentDir)
		fp.historyIdx = len(fp.history) - 1
	}
	return true
}

func (fp *FilePicker) canGoBack() bool {
	return fp.historyIdx > 0 && fp.historyIdx < len(fp.history)
}

func (fp *FilePicker) canGoForward() bool {
	return fp.historyIdx >= 0 && fp.historyIdx+1 < len(fp.history)
}

func (fp *FilePicker) goBack(e *Editor) {
	if !fp.canGoBack() {
		return
	}
	target := fp.history[fp.historyIdx-1]
	if fp.loadDir(target, "") {
		fp.historyIdx--
		if e != nil {
			e.lastPickerDir = fp.currentDir
			e.watchPickerDir(fp.currentDir)
		}
	}
}

func (fp *FilePicker) goForward(e *Editor) {
	if !fp.canGoForward() {
		return
	}
	target := fp.history[fp.historyIdx+1]
	if fp.loadDir(target, "") {
		fp.historyIdx++
		if e != nil {
			e.lastPickerDir = fp.currentDir
			e.watchPickerDir(fp.currentDir)
		}
	}
}

func (fp *FilePicker) applyFilter(preferName string) {
	query := strings.ToLower(strings.TrimSpace(fp.filterInput.text()))
	fp.entries = fp.entries[:0]
	fp.dirCount = 0
	fp.fileCount = 0
	fp.totalFileBytes = 0

	for _, ent := range fp.allEntries {
		if ent.IsParent {
			if query == "" {
				fp.entries = append(fp.entries, ent)
			}
			continue
		}
		if !fp.showHidden && ent.IsHidden {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(ent.Name), query) {
			continue
		}
		fp.entries = append(fp.entries, ent)
		if ent.IsDir {
			fp.dirCount++
		} else {
			fp.fileCount++
			fp.totalFileBytes += ent.Size
		}
	}

	fp.cursor = 0
	fp.scrollOff = 0
	if preferName != "" {
		for i, ent := range fp.entries {
			if ent.Name == preferName {
				fp.cursor = i
				break
			}
		}
	}
	fp.ensureCursorVisible()
	if fp.mode == fpModeOpen || fp.isFilenameTrackingDirOrEmpty() {
		fp.syncFilenamePreview()
	}
}

func (fp *FilePicker) visibleListRows() int {
	if fp.listY2 >= fp.listY1 {
		return fp.listY2 - fp.listY1 + 1
	}
	return 10
}

// clampScroll 은 커맨드 팔레트(drawMenu)와 동일하게 scrollOff 를 [0, maxOff] 범위로만 제한한다.
// 렌더링 중에 커서로 강제 스냅하지 않으므로 마우스 휠 스크롤 위치가 그대로 유지된다.
func (fp *FilePicker) clampScroll() {
	if len(fp.entries) == 0 {
		fp.cursor = 0
		fp.scrollOff = 0
		return
	}
	if fp.cursor < 0 {
		fp.cursor = 0
	}
	if fp.cursor >= len(fp.entries) {
		fp.cursor = len(fp.entries) - 1
	}
	vis := fp.visibleListRows()
	if vis <= 0 {
		vis = 1
	}
	if fp.scrollOff < 0 {
		fp.scrollOff = 0
	}
	maxOff := len(fp.entries) - vis
	if maxOff < 0 {
		maxOff = 0
	}
	if fp.scrollOff > maxOff {
		fp.scrollOff = maxOff
	}
}

func (fp *FilePicker) clampPlacesScroll() {
	if len(fp.places) == 0 {
		fp.placeCursor = 0
		fp.placesScrollOff = 0
		return
	}
	if fp.placeCursor < 0 {
		fp.placeCursor = 0
	}
	if fp.placeCursor >= len(fp.places) {
		fp.placeCursor = len(fp.places) - 1
	}
	vis := fp.visibleListRows()
	if vis <= 0 {
		vis = 1
	}
	if fp.placesScrollOff < 0 {
		fp.placesScrollOff = 0
	}
	maxOff := len(fp.places) - vis
	if maxOff < 0 {
		maxOff = 0
	}
	if fp.placesScrollOff > maxOff {
		fp.placesScrollOff = maxOff
	}
}

// ensureCursorVisible 은 키보드 이동/필터링 시에만 호출되어 선택 커서를 화면 안에 맞춘다.
func (fp *FilePicker) ensureCursorVisible() {
	fp.clampScroll()
	if len(fp.entries) == 0 {
		return
	}
	vis := fp.visibleListRows()
	if vis <= 0 {
		vis = 1
	}
	if fp.cursor < fp.scrollOff {
		fp.scrollOff = fp.cursor
	} else if fp.cursor >= fp.scrollOff+vis {
		fp.scrollOff = fp.cursor - vis + 1
	}
	fp.clampScroll()
}

func (fp *FilePicker) ensurePlaceCursorVisible() {
	fp.clampPlacesScroll()
	if len(fp.places) == 0 {
		return
	}
	vis := fp.visibleListRows()
	if vis <= 0 {
		vis = 1
	}
	if fp.placeCursor < fp.placesScrollOff {
		fp.placesScrollOff = fp.placeCursor
	} else if fp.placeCursor >= fp.placesScrollOff+vis {
		fp.placesScrollOff = fp.placeCursor - vis + 1
	}
	fp.clampPlacesScroll()
}

func (fp *FilePicker) goParent(e *Editor) {
	parent := filepath.Dir(fp.currentDir)
	if parent == fp.currentDir {
		return
	}
	prevName := filepath.Base(fp.currentDir)
	fp.navigateTo(e, parent, prevName)
}

func (fp *FilePicker) goHome(e *Editor) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	fp.navigateTo(e, home, "")
}

func (fp *FilePicker) resolveUserPath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	expanded := expandHomePath(trimmed)
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	return filepath.Clean(filepath.Join(fp.currentDir, expanded))
}

// --- [ Editor 연동 메서드 ] ---

func (e *Editor) defaultPickerStartDir() string {
	b := e.getActive()
	if b != nil && b.filePath != "" && !b.isConfig {
		dir := filepath.Dir(b.filePath)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	if e.lastPickerDir != "" {
		if info, err := os.Stat(e.lastPickerDir); err == nil && info.IsDir() {
			return e.lastPickerDir
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return string(filepath.Separator)
}

func (e *Editor) openFilePickerModal(mode FilePickerMode, startDir, defaultName string, readOnly bool, onSelect func(string), onCancel func()) {
	e.paletteActive = false
	e.ctxMenuActive = false
	e.encodeMenuActive = false
	e.encodeMenuState = 0
	e.langMenuActive = false
	e.closeKeybindMenu()

	if startDir == "" {
		startDir = e.defaultPickerStartDir()
	}
	fp := newFilePicker(mode, startDir, defaultName, readOnly, onSelect, onCancel)
	e.lastPickerDir = fp.currentDir
	e.filePicker = fp
	e.watchPickerDir(fp.currentDir)
	e.needsFullRefresh = true
}

func (e *Editor) closeFilePicker(cancelled bool) {
	if e.filePicker == nil {
		return
	}
	fp := e.filePicker
	e.filePicker = nil
	e.unwatchPickerDir()
	e.needsFullRefresh = true
	if cancelled && fp.onCancel != nil {
		fp.onCancel()
	}
}

func (e *Editor) openConfirmDialog(title, message, yesLabel, noLabel string, defaultYes bool, onConfirm, onCancel func()) {
	e.confirmDialog = &ConfirmDialog{
		title:     title,
		message:   message,
		yesLabel:  yesLabel,
		noLabel:   noLabel,
		focusYes:  defaultYes,
		onConfirm: onConfirm,
		onCancel:  onCancel,
	}
	e.needsFullRefresh = true
}

func (e *Editor) confirmFilePickerSelection(fp *FilePicker, targetPath string) {
	if targetPath == "" {
		fp.errorMsg = T("fp.err.empty_name")
		return
	}
	if fp.mode == fpModeOpen {
		info, err := os.Stat(targetPath)
		if err != nil {
			fp.errorMsg = Tf("fp.err.not_found", filepath.Base(targetPath))
			return
		}
		if info.IsDir() {
			if fp.navigateTo(e, targetPath, "") {
				fp.syncFilenamePreview()
			}
			return
		}
		e.lastPickerDir = filepath.Dir(targetPath)
		onSel := fp.onSelect
		e.filePicker = nil
		e.unwatchPickerDir()
		e.needsFullRefresh = true
		if onSel != nil {
			onSel(targetPath)
		}
		return
	}

	// Save As 모드
	if info, err := os.Stat(targetPath); err == nil {
		if info.IsDir() {
			if fp.navigateTo(e, targetPath, "") {
				fp.syncFilenamePreview()
			}
			return
		}
		onSel := fp.onSelect
		e.openConfirmDialog(
			" "+T("dialog.overwrite.title")+" ",
			Tf("dialog.overwrite.body", filepath.Base(targetPath)),
			T("dialog.overwrite.ok"),
			T("dialog.overwrite.cancel"),
			false,
			func() {
				e.lastPickerDir = filepath.Dir(targetPath)
				e.filePicker = nil
				e.unwatchPickerDir()
				e.needsFullRefresh = true
				if onSel != nil {
					onSel(targetPath)
				}
			},
			func() {
				e.needsFullRefresh = true
			},
		)
		return
	}

	parentDir := filepath.Dir(targetPath)
	if info, err := os.Stat(parentDir); err != nil || !info.IsDir() {
		fp.errorMsg = Tf("fp.err.no_parent", parentDir)
		return
	}

	e.lastPickerDir = parentDir
	onSel := fp.onSelect
	e.filePicker = nil
	e.unwatchPickerDir()
	e.needsFullRefresh = true
	if onSel != nil {
		onSel(targetPath)
	}
}

func (e *Editor) activateSelectedEntry(fp *FilePicker) {
	if len(fp.entries) == 0 || fp.cursor < 0 || fp.cursor >= len(fp.entries) {
		return
	}
	ent := fp.entries[fp.cursor]
	if ent.IsParent {
		fp.goParent(e)
		return
	}
	if ent.IsDir {
		fp.navigateTo(e, ent.Path, "")
		return
	}
	if fp.mode == fpModeSaveAs {
		fp.filenameInput.set(ent.Name)
	}
	e.confirmFilePickerSelection(fp, ent.Path)
}

func (e *Editor) submitFilePickerAction(fp *FilePicker) {
	if fp.focus == fpFocusNewDir {
		e.createNewDirInPicker(fp)
		return
	}
	if fp.focus == fpFocusPath {
		e.submitPathInput(fp)
		return
	}
	if fp.focus == fpFocusPlaces {
		if fp.placeCursor >= 0 && fp.placeCursor < len(fp.places) {
			fp.navigateTo(e, fp.places[fp.placeCursor].Path, "")
			fp.focus = fpFocusList
		}
		return
	}
	if fp.mode == fpModeSaveAs {
		raw := strings.TrimSpace(fp.filenameInput.text())
		if raw != "" {
			e.confirmFilePickerSelection(fp, fp.resolveUserPath(raw))
			return
		}
		e.activateSelectedEntry(fp)
		return
	}

	if fp.focus == fpFocusFilename {
		raw := strings.TrimSpace(fp.filenameInput.text())
		if raw != "" {
			e.confirmFilePickerSelection(fp, fp.resolveUserPath(raw))
			return
		}
	}
	e.activateSelectedEntry(fp)
}

func (e *Editor) submitPathInput(fp *FilePicker) {
	target := fp.resolveUserPath(fp.pathInput.text())
	if target == "" {
		fp.focus = fp.prevFocus
		return
	}
	info, err := os.Stat(target)
	if err != nil {
		fp.errorMsg = Tf("fp.err.path_not_found", target)
		return
	}
	if info.IsDir() {
		if fp.navigateTo(e, target, "") {
			if fp.mode == fpModeSaveAs {
				fp.focus = fpFocusFilename
			} else {
				fp.focus = fpFocusList
			}
		}
		return
	}
	if fp.mode == fpModeOpen {
		e.confirmFilePickerSelection(fp, target)
	} else {
		parent := filepath.Dir(target)
		if fp.navigateTo(e, parent, filepath.Base(target)) {
			fp.filenameInput.set(filepath.Base(target))
			fp.focus = fpFocusFilename
		}
	}
}

func (e *Editor) createNewDirInPicker(fp *FilePicker) {
	name := strings.TrimSpace(fp.newDirInput.text())
	if name == "" {
		fp.errorMsg = T("fp.err.empty_dir_name")
		return
	}
	if name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '/') {
		fp.errorMsg = T("fp.err.invalid_dir_name")
		return
	}
	newPath := filepath.Join(fp.currentDir, name)
	if err := os.Mkdir(newPath, 0755); err != nil {
		fp.errorMsg = Tf("fp.err.mkdir_failed", err)
		return
	}
	fp.newDirInput.clear()
	if strings.HasPrefix(name, ".") {
		fp.showHidden = true
	}
	fp.loadDir(fp.currentDir, name)
	fp.syncFilenamePreview()
	fp.focus = fp.prevFocus
	if fp.focus == fpFocusNewDir {
		fp.focus = fpFocusList
	}
	e.needsFullRefresh = true
}

func (fp *FilePicker) autocompletePath() {
	raw := fp.pathInput.text()
	if raw == "" {
		return
	}
	expanded := expandHomePath(raw)
	dir := filepath.Dir(expanded)
	prefix := filepath.Base(expanded)
	if strings.HasSuffix(raw, string(filepath.Separator)) || strings.HasSuffix(raw, "/") {
		dir = expanded
		prefix = ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var matches []string
	var isDirMatch []bool
	for _, de := range entries {
		name := de.Name()
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
			isDir := de.IsDir()
			if de.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
					isDir = st.IsDir()
				}
			}
			isDirMatch = append(isDirMatch, isDir)
		}
	}
	if len(matches) == 0 {
		return
	}
	if len(matches) == 1 {
		completed := filepath.Join(dir, matches[0])
		if isDirMatch[0] {
			completed += string(filepath.Separator)
		}
		fp.pathInput.set(completed)
		return
	}
	lcp := matches[0]
	for _, m := range matches[1:] {
		i := 0
		for i < len(lcp) && i < len(m) && lcp[i] == m[i] {
			i++
		}
		lcp = lcp[:i]
	}
	if len(lcp) > len(prefix) {
		fp.pathInput.set(filepath.Join(dir, lcp))
	}
}

// --- [ 키보드 이벤트 처리 ] ---

func (e *Editor) handleConfirmKey(ev *tcell.EventKey) bool {
	cd := e.confirmDialog
	if cd == nil {
		return false
	}
	switch ev.Key() {
	case tcell.KeyEscape:
		onCancel := cd.onCancel
		e.confirmDialog = nil
		e.needsFullRefresh = true
		if onCancel != nil {
			onCancel()
		}
		return true
	case tcell.KeyLeft, tcell.KeyRight, tcell.KeyTab, tcell.KeyBacktab:
		cd.focusYes = !cd.focusYes
		e.needsFullRefresh = true
		return true
	case tcell.KeyEnter:
		confirm := cd.focusYes
		onConfirm, onCancel := cd.onConfirm, cd.onCancel
		e.confirmDialog = nil
		e.needsFullRefresh = true
		if confirm {
			if onConfirm != nil {
				onConfirm()
			}
		} else {
			if onCancel != nil {
				onCancel()
			}
		}
		return true
	case tcell.KeyRune:
		r := unicode.ToLower(ev.Rune())
		if r == 'y' {
			onConfirm := cd.onConfirm
			e.confirmDialog = nil
			e.needsFullRefresh = true
			if onConfirm != nil {
				onConfirm()
			}
			return true
		}
		if r == 'n' {
			onCancel := cd.onCancel
			e.confirmDialog = nil
			e.needsFullRefresh = true
			if onCancel != nil {
				onCancel()
			}
			return true
		}
	}
	return true
}

func (e *Editor) handleConfirmMouse(mx, my int, isNewPress, mouseMoved bool) bool {
	cd := e.confirmDialog
	if cd == nil {
		return false
	}
	if mouseMoved {
		if cd.yesBtn.hit(mx, my) {
			cd.focusYes = true
		} else if cd.noBtn.hit(mx, my) {
			cd.focusYes = false
		}
	}
	if isNewPress {
		if cd.yesBtn.hit(mx, my) {
			onConfirm := cd.onConfirm
			e.confirmDialog = nil
			e.needsFullRefresh = true
			if onConfirm != nil {
				onConfirm()
			}
			return true
		}
		inBox := mx >= cd.boxX && mx < cd.boxX+cd.boxW && my >= cd.boxY && my < cd.boxY+cd.boxH
		if cd.noBtn.hit(mx, my) || !inBox {
			onCancel := cd.onCancel
			e.confirmDialog = nil
			e.needsFullRefresh = true
			if onCancel != nil {
				onCancel()
			}
			return true
		}
	}
	return true
}

// --- [ 리매핑 가능한 파일 피커 단축키 액션 ] ---

func (e *Editor) fpActionBack() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	fp.goBack(e)
	if fp.focus == fpFocusPath {
		fp.pathInput.set(fp.currentDir)
	}
}

func (e *Editor) fpActionForward() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	fp.goForward(e)
	if fp.focus == fpFocusPath {
		fp.pathInput.set(fp.currentDir)
	}
}

func (e *Editor) fpActionParent() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	fp.goParent(e)
	if fp.focus == fpFocusPath {
		fp.pathInput.set(fp.currentDir)
	}
}

func (e *Editor) fpActionHome() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	fp.goHome(e)
	if fp.focus == fpFocusPath {
		fp.pathInput.set(fp.currentDir)
	}
}

func (e *Editor) fpActionRefresh() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	fp.refreshDirKeepState()
}

func (e *Editor) fpActionToggleHidden() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	curName := ""
	if fp.cursor >= 0 && fp.cursor < len(fp.entries) {
		curName = fp.entries[fp.cursor].Name
	}
	fp.showHidden = !fp.showHidden
	fp.applyFilter(curName)
}

func (e *Editor) fpActionEditPath() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	if fp.focus != fpFocusPath {
		fp.prevFocus = fp.focus
		fp.focus = fpFocusPath
		fp.pathInput.set(fp.currentDir)
	} else {
		fp.focus = fp.prevFocus
	}
}

func (e *Editor) fpActionFilter() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	if fp.focus != fpFocusFilter {
		fp.focus = fpFocusFilter
	} else {
		fp.focus = fpFocusList
	}
}

func (e *Editor) fpActionNewDir() {
	fp := e.filePicker
	if fp == nil {
		return
	}
	if fp.focus != fpFocusNewDir {
		fp.prevFocus = fp.focus
		fp.focus = fpFocusNewDir
		fp.newDirInput.clear()
	} else {
		fp.focus = fp.prevFocus
	}
}

func (e *Editor) handleFilePickerKey(ev *tcell.EventKey) bool {
	fp := e.filePicker
	if fp == nil {
		return false
	}

	k := ev.Key()
	fp.errorMsg = ""

	// 0. Esc 는 예약 키로 항상 취소/복귀 동작
	if k == tcell.KeyEscape {
		if fp.focus == fpFocusPath || fp.focus == fpFocusNewDir {
			fp.focus = fp.prevFocus
			if fp.focus == fpFocusPath || fp.focus == fpFocusNewDir {
				fp.focus = fpFocusList
			}
			return true
		}
		if fp.filterInput.text() != "" {
			fp.filterInput.clear()
			fp.applyFilter("")
			return true
		}
		e.closeFilePicker(true)
		return true
	}

	// 1. 사용자 설정으로 리매핑 가능한 파일 피커 전용 단축키 (fpBindings)
	evChord := chordFromEvent(ev)
	if act := e.fpBindings[evChord]; act != nil {
		act.Fn(e, nil)
		return true
	}

	if k == tcell.KeyCtrlV {
		fp.pasteText(globalClipboard.ReadSync())
		return true
	}

	// 2. 경로 직접 입력 모드
	if fp.focus == fpFocusPath {
		switch k {
		case tcell.KeyEnter:
			e.submitPathInput(fp)
		case tcell.KeyTab:
			fp.autocompletePath()
		default:
			fp.pathInput.handleKey(ev)
		}
		return true
	}

	// 3. 새 폴더 입력 모드
	if fp.focus == fpFocusNewDir {
		switch k {
		case tcell.KeyEnter:
			e.createNewDirInPicker(fp)
		case tcell.KeyTab, tcell.KeyBacktab:
			fp.focus = fp.prevFocus
			if fp.focus == fpFocusNewDir {
				fp.focus = fpFocusList
			}
		default:
			fp.newDirInput.handleKey(ev)
		}
		return true
	}

	// 4. Tab / Shift+Tab 포커스 순환
	if k == tcell.KeyTab || k == tcell.KeyBacktab {
		var order []FilePickerFocus
		if fp.mode == fpModeSaveAs {
			if fp.showPlaces {
				order = []FilePickerFocus{fpFocusFilename, fpFocusList, fpFocusPlaces, fpFocusFilter}
			} else {
				order = []FilePickerFocus{fpFocusFilename, fpFocusList, fpFocusFilter}
			}
		} else {
			if fp.showPlaces {
				order = []FilePickerFocus{fpFocusList, fpFocusPlaces, fpFocusFilter, fpFocusFilename}
			} else {
				order = []FilePickerFocus{fpFocusList, fpFocusFilter, fpFocusFilename}
			}
		}
		idx := 0
		for i, f := range order {
			if f == fp.focus {
				idx = i
				break
			}
		}
		if k == tcell.KeyTab {
			idx = (idx + 1) % len(order)
		} else {
			idx = (idx - 1 + len(order)) % len(order)
		}
		fp.focus = order[idx]
		return true
	}

	// 5. 좌측 Places 사이드바 포커스 시 키 처리
	if fp.focus == fpFocusPlaces {
		pageStep := fp.visibleListRows() - 1
		if pageStep < 3 {
			pageStep = 3
		}
		switch k {
		case tcell.KeyUp:
			if len(fp.places) > 0 {
				fp.placeCursor--
				if fp.placeCursor < 0 {
					fp.placeCursor = len(fp.places) - 1
				}
				fp.ensurePlaceCursorVisible()
			}
		case tcell.KeyDown:
			if len(fp.places) > 0 {
				fp.placeCursor++
				if fp.placeCursor >= len(fp.places) {
					fp.placeCursor = 0
				}
				fp.ensurePlaceCursorVisible()
			}
		case tcell.KeyPgUp:
			if len(fp.places) > 0 {
				fp.placeCursor -= pageStep
				if fp.placeCursor < 0 {
					fp.placeCursor = 0
				}
				fp.ensurePlaceCursorVisible()
			}
		case tcell.KeyPgDn:
			if len(fp.places) > 0 {
				fp.placeCursor += pageStep
				if fp.placeCursor >= len(fp.places) {
					fp.placeCursor = len(fp.places) - 1
				}
				fp.ensurePlaceCursorVisible()
			}
		case tcell.KeyHome:
			fp.placeCursor = 0
			fp.ensurePlaceCursorVisible()
		case tcell.KeyEnd:
			if len(fp.places) > 0 {
				fp.placeCursor = len(fp.places) - 1
				fp.ensurePlaceCursorVisible()
			}
		case tcell.KeyEnter, tcell.KeyRight:
			if fp.placeCursor >= 0 && fp.placeCursor < len(fp.places) {
				fp.navigateTo(e, fp.places[fp.placeCursor].Path, "")
				fp.focus = fpFocusList
			}
		}
		return true
	}

	// 6. 목록 탐색 키(Up/Down/PgUp/PgDn)는 필터나 파일명 입력 중에도 즉시 동작한다.
	pageStep := fp.visibleListRows() - 1
	if pageStep < 3 {
		pageStep = 3
	}
	switch k {
	case tcell.KeyUp:
		if len(fp.entries) > 0 {
			fp.cursor--
			if fp.cursor < 0 {
				fp.cursor = len(fp.entries) - 1
			}
			fp.ensureCursorVisible()
			fp.syncFilenamePreview()
		}
		return true
	case tcell.KeyDown:
		if len(fp.entries) > 0 {
			fp.cursor++
			if fp.cursor >= len(fp.entries) {
				fp.cursor = 0
			}
			fp.ensureCursorVisible()
			fp.syncFilenamePreview()
		}
		return true
	case tcell.KeyPgUp:
		if len(fp.entries) > 0 {
			fp.cursor -= pageStep
			if fp.cursor < 0 {
				fp.cursor = 0
			}
			fp.ensureCursorVisible()
			fp.syncFilenamePreview()
		}
		return true
	case tcell.KeyPgDn:
		if len(fp.entries) > 0 {
			fp.cursor += pageStep
			if fp.cursor >= len(fp.entries) {
				fp.cursor = len(fp.entries) - 1
			}
			fp.ensureCursorVisible()
			fp.syncFilenamePreview()
		}
		return true
	}

	// 7. 포커스별 키 처리
	switch fp.focus {
	case fpFocusList:
		switch k {
		case tcell.KeyHome:
			fp.cursor = 0
			fp.ensureCursorVisible()
			fp.syncFilenamePreview()
		case tcell.KeyEnd:
			if len(fp.entries) > 0 {
				fp.cursor = len(fp.entries) - 1
				fp.ensureCursorVisible()
				fp.syncFilenamePreview()
			}
		case tcell.KeyEnter:
			e.activateSelectedEntry(fp)
		case tcell.KeyRight:
			if fp.cursor >= 0 && fp.cursor < len(fp.entries) && fp.entries[fp.cursor].IsDir {
				e.activateSelectedEntry(fp)
			}
		case tcell.KeyLeft:
			fp.goParent(e)
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			if fp.filterInput.text() != "" {
				fp.filterInput.handleKey(ev)
				fp.applyFilter("")
			} else {
				fp.goParent(e)
			}
		case tcell.KeyRune:
			r := ev.Rune()
			if r == '~' && fp.filterInput.text() == "" {
				fp.goHome(e)
				return true
			}
			if unicode.IsPrint(r) {
				if fp.mode == fpModeSaveAs {
					fp.focus = fpFocusFilename
					fp.filenameInput.handleKey(ev)
				} else {
					fp.filterInput.handleKey(ev)
					fp.applyFilter("")
				}
			}
		}

	case fpFocusFilter:
		switch k {
		case tcell.KeyEnter:
			e.activateSelectedEntry(fp)
		default:
			before := fp.filterInput.text()
			fp.filterInput.handleKey(ev)
			if fp.filterInput.text() != before {
				fp.applyFilter("")
			}
		}

	case fpFocusFilename:
		switch k {
		case tcell.KeyEnter:
			e.submitFilePickerAction(fp)
		default:
			fp.filenameInput.handleKey(ev)
		}
	}
	return true
}

// syncFilenamePreview 는 키보드/마우스로 항목이 바뀔 때 하단 이름 바를 즉시 동기화한다.
// 파일뿐만 아니라 디렉토리(이름/) 및 상위 폴더(..)도 함께 반영한다.
func (fp *FilePicker) syncFilenamePreview() {
	if fp.cursor < 0 || fp.cursor >= len(fp.entries) {
		fp.filenameInput.clear()
		return
	}
	ent := fp.entries[fp.cursor]
	if ent.IsParent {
		fp.filenameInput.set("..")
	} else if ent.IsDir {
		fp.filenameInput.set(ent.Name + "/")
	} else {
		fp.filenameInput.set(ent.Name)
	}
}

// --- [ 마우스 이벤트 처리 (커맨드 팔레트 방식 스크롤 + Dolphin 2패널) ] ---

func (fp *FilePicker) scrollFromTrackY(my int) {
	vis := fp.visibleListRows()
	maxOff := len(fp.entries) - vis
	if maxOff <= 0 || vis <= 1 {
		fp.scrollOff = 0
		return
	}
	rel := my - fp.listY1
	if rel < 0 {
		rel = 0
	}
	if rel >= vis {
		rel = vis - 1
	}
	fp.scrollOff = rel * maxOff / (vis - 1)
	fp.clampScroll()
}

func (e *Editor) handleFilePickerMouse(mx, my int, buttons tcell.ButtonMask, isNewPress, isDrag, isWheel, mouseMoved bool, clickCount *int) bool {
	fp := e.filePicker
	if fp == nil {
		return false
	}

	inBox := mx >= fp.boxX && mx < fp.boxX+fp.boxW && my >= fp.boxY && my < fp.boxY+fp.boxH

	// 1. 휠 스크롤 (커맨드 팔레트와 동일: 3줄 단위 이동 + 범위 클램핑만 수행)
	if isWheel {
		if inBox {
			inPlaces := fp.showPlaces && mx >= fp.placesX1 && mx <= fp.placesX2 && my >= fp.listY1 && my <= fp.listY2
			if inPlaces {
				if buttons&tcell.WheelUp != 0 {
					fp.placesScrollOff -= 3
				} else if buttons&tcell.WheelDown != 0 {
					fp.placesScrollOff += 3
				}
				fp.clampPlacesScroll()
			} else {
				if buttons&tcell.WheelUp != 0 {
					fp.scrollOff -= 3
				} else if buttons&tcell.WheelDown != 0 {
					fp.scrollOff += 3
				}
				fp.clampScroll()
			}
		}
		return true
	}

	// 2. 스크롤바 드래그 처리
	if isDrag && fp.draggingScrollbar {
		fp.scrollFromTrackY(my)
		return true
	}
	if !isDrag && !isNewPress {
		fp.draggingScrollbar = false
	}

	// 3. 마우스 호버 하이라이트 (선택이나 Save As 파일명은 건드리지 않음)
	if mouseMoved {
		fp.hoverListIdx = -1
		fp.hoverPlaceIdx = -1
		if my >= fp.listY1 && my <= fp.listY2 {
			if fp.showPlaces && mx >= fp.placesX1 && mx <= fp.placesX2 {
				idx := fp.placesScrollOff + (my - fp.listY1)
				if idx >= 0 && idx < len(fp.places) {
					fp.hoverPlaceIdx = idx
				}
			} else if mx >= fp.listX1 && mx <= fp.listX2 {
				idx := fp.scrollOff + (my - fp.listY1)
				if idx >= 0 && idx < len(fp.entries) {
					fp.hoverListIdx = idx
				}
			}
		}
	}

	if !isNewPress {
		return true
	}

	// 모달 바깥 클릭 -> 취소
	if !inBox {
		e.closeFilePicker(true)
		return true
	}

	fp.errorMsg = ""

	// 4. Dolphin 상단 툴바: [←] [→] [↑] / [+ 폴더] / [x] .* 숨김
	if fp.backBtn.hit(mx, my) {
		fp.goBack(e)
		if fp.focus == fpFocusPath {
			fp.pathInput.set(fp.currentDir)
		}
		return true
	}
	if fp.fwdBtn.hit(mx, my) {
		fp.goForward(e)
		if fp.focus == fpFocusPath {
			fp.pathInput.set(fp.currentDir)
		}
		return true
	}
	if fp.upBtn.hit(mx, my) {
		fp.goParent(e)
		if fp.focus == fpFocusPath {
			fp.pathInput.set(fp.currentDir)
		}
		return true
	}
	if fp.hiddenChk.hit(mx, my) {
		curName := ""
		if fp.cursor >= 0 && fp.cursor < len(fp.entries) {
			curName = fp.entries[fp.cursor].Name
		}
		fp.showHidden = !fp.showHidden
		fp.applyFilter(curName)
		return true
	}
	if fp.newDirBtn.hit(mx, my) {
		if fp.focus != fpFocusNewDir {
			fp.prevFocus = fp.focus
			fp.focus = fpFocusNewDir
			fp.newDirInput.clear()
		} else {
			fp.focus = fp.prevFocus
		}
		return true
	}

	// 6. 경로 바 (편집 모드 vs Dolphin 브레드크럼)
	if fp.focus == fpFocusPath && fp.pathBox.hit(mx, my) {
		fp.pathInput.clickAt(mx, fp.pathBox.X1, fp.pathBox.X2-fp.pathBox.X1+1)
		return true
	}
	for _, cr := range fp.crumbs {
		if cr.Rect.hit(mx, my) {
			fp.navigateTo(e, cr.Path, "")
			return true
		}
	}
	if fp.pathBox.hit(mx, my) {
		fp.prevFocus = fp.focus
		fp.focus = fpFocusPath
		fp.pathInput.set(fp.currentDir)
		return true
	}

	// 7. 컬럼 헤더 클릭 -> 정렬 전환 (이름 / 크기 / 수정일)
	if fp.colNameHdr.hit(mx, my) {
		fp.toggleSort(fpSortName)
		return true
	}
	if fp.colSizeHdr.hit(mx, my) {
		fp.toggleSort(fpSortSize)
		return true
	}
	if fp.colDateHdr.hit(mx, my) {
		fp.toggleSort(fpSortModTime)
		return true
	}

	// 8. 스크롤 화살표 ▲ / ▼ (커맨드 팔레트 방식 페이지 스크롤)
	pageStep := fp.visibleListRows() - 1
	if pageStep < 3 {
		pageStep = 3
	}
	if fp.scrollUpBtn.hit(mx, my) {
		fp.scrollOff -= pageStep
		fp.clampScroll()
		return true
	}
	if fp.scrollDownBtn.hit(mx, my) {
		fp.scrollOff += pageStep
		fp.clampScroll()
		return true
	}

	// 9. 스크롤바 트랙 클릭
	if fp.scrollbarX >= 0 && mx == fp.scrollbarX && my >= fp.listY1 && my <= fp.listY2 {
		fp.draggingScrollbar = true
		fp.scrollFromTrackY(my)
		return true
	}

	// 10. 필터 입력창 클릭
	if fp.filterBox.hit(mx, my) {
		fp.focus = fpFocusFilter
		fp.filterInput.clickAt(mx, fp.filterBox.X1, fp.filterBox.X2-fp.filterBox.X1+1)
		return true
	}

	// 11. 하단 액션 버튼 & 입력창 ([ 열기/저장/생성 ] / [ 취소 ])
	if fp.okBtn.hit(mx, my) {
		e.submitFilePickerAction(fp)
		return true
	}
	if fp.cancelBtn.hit(mx, my) {
		if fp.focus == fpFocusNewDir {
			fp.focus = fp.prevFocus
			if fp.focus == fpFocusNewDir {
				fp.focus = fpFocusList
			}
			return true
		}
		e.closeFilePicker(true)
		return true
	}
	if fp.bottomInputBox.hit(mx, my) {
		if fp.focus == fpFocusNewDir {
			fp.newDirInput.clickAt(mx, fp.bottomInputBox.X1, fp.bottomInputBox.X2-fp.bottomInputBox.X1+1)
		} else {
			fp.focus = fpFocusFilename
			fp.filenameInput.clickAt(mx, fp.bottomInputBox.X1, fp.bottomInputBox.X2-fp.bottomInputBox.X1+1)
		}
		return true
	}

	// 12. 좌측 위치(Places) 사이드바 클릭 -> 단일 클릭으로 즉시 이동 (Dolphin 동작)
	if fp.showPlaces && my >= fp.listY1 && my <= fp.listY2 && mx >= fp.placesX1 && mx <= fp.placesX2 {
		idx := fp.placesScrollOff + (my - fp.listY1)
		if idx >= 0 && idx < len(fp.places) {
			fp.placeCursor = idx
			fp.focus = fpFocusList
			fp.navigateTo(e, fp.places[idx].Path, "")
		}
		return true
	}

	// 13. 우측 파일 목록 클릭 (단일 클릭 = 선택, 더블 클릭 = 폴더 진입 / 파일 열기·저장)
	if my >= fp.listY1 && my <= fp.listY2 && mx >= fp.listX1 && mx <= fp.listX2 {
		fp.focus = fpFocusList
		idx := fp.scrollOff + (my - fp.listY1)
		if idx >= 0 && idx < len(fp.entries) {
			fp.cursor = idx
			fp.syncFilenamePreview()
			if clickCount != nil && *clickCount >= 2 {
				*clickCount = 0
				e.activateSelectedEntry(fp)
			}
		}
		return true
	}

	return true
}

// --- [ 렌더링 ] ---

func clearDoubleWidthLeftEdge(s tcell.Screen, setCell func(int, int, rune, []rune, tcell.Style), bx, by, bh, screenW, screenH int) {
	lx := bx - 1
	if lx < 0 || lx >= screenW {
		return
	}
	for y := 0; y < bh; y++ {
		ly := by + y
		if ly >= 0 && ly < screenH {
			mainc, _, _, width := s.GetContent(lx, ly)
			if width == 2 || runewidth.RuneWidth(mainc) == 2 {
				setCell(lx, ly, ' ', nil, tcell.StyleDefault)
			}
		}
	}
}

func drawTextAt(setCell func(int, int, rune, []rune, tcell.Style), x, y, maxX int, text string, style tcell.Style) int {
	cx := x
	for _, r := range text {
		rw := runewidth.RuneWidth(r)
		if cx+rw > maxX {
			break
		}
		setCell(cx, y, r, nil, style)
		cx += rw
	}
	return cx
}

func (e *Editor) drawFilePicker(s tcell.Screen, w, h int) {
	fp := e.filePicker
	if fp == nil {
		return
	}
	fp.cursorX, fp.cursorY = -1, -1

	setCell := func(x, y int, r rune, comb []rune, style tcell.Style) {
		if x >= 0 && x < w && y >= 0 && y < h {
			s.SetContent(x, y, r, comb, style)
		}
	}

	// Dolphin 2패널 반응형 크기 계산
	boxW := w * 90 / 100
	if boxW < 68 {
		boxW = 68
	}
	if boxW > 120 {
		boxW = 120
	}
	if boxW > w-2 {
		boxW = w - 2
	}
	boxH := h * 84 / 100
	if boxH < 15 {
		boxH = 15
	}
	if boxH > 36 {
		boxH = 36
	}
	if boxH > h-2 {
		boxH = h - 2
	}
	if boxW < 24 || boxH < 9 {
		return
	}

	boxX := (w - boxW) / 2
	boxY := (h - boxH) / 2
	fp.boxX, fp.boxY, fp.boxW, fp.boxH = boxX, boxY, boxW, boxH

	// 좌측 Places 사이드바 표시 여부 (폭 66 이상일 때 표시)
	fp.showPlaces = boxW >= 66
	sidebarW := 18
	if boxW >= 96 {
		sidebarW = 20
	}
	splitX := boxX
	if fp.showPlaces {
		fp.placesX1 = boxX + 1
		fp.placesX2 = boxX + sidebarW
		splitX = fp.placesX2 + 1
		fp.listX1 = splitX + 1
	} else {
		fp.placesX1, fp.placesX2 = 1, 0
		fp.listX1 = boxX + 1
	}
	fp.listX2 = boxX + boxW - 2
	fp.listY1 = boxY + 4
	fp.listY2 = boxY + boxH - 5
	if fp.listY2 < fp.listY1 {
		fp.listY2 = fp.listY1
	}

	// 💡 핵심 수정: 커맨드 팔레트와 동일하게 렌더링 시에는 범위 클램핑만 수행한다.
	// ensureCursorVisible()을 여기서 부르면 마우스 휠 스크롤이 매 프레임 커서로 되돌아간다.
	fp.clampScroll()
	fp.clampPlacesScroll()

	clearDoubleWidthLeftEdge(s, setCell, boxX, boxY, boxH, w, h)

	// 스타일 정의 (포커스된 영역과 비포커스 영역의 색상을 뚜렷하게 구분)
	borderStyle := tcell.StyleDefault.Background(tcell.ColorDarkBlue).Foreground(tcell.ColorWhite)
	activeDividerStyle := tcell.StyleDefault.Background(tcell.ColorDarkBlue).Foreground(tcell.ColorAqua).Bold(true)
	titleStyle := borderStyle.Bold(true)
	bgStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite)
	sidebarBgStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorSilver)
	headerStyle := tcell.StyleDefault.Background(tcell.ColorNavy).Foreground(tcell.ColorSilver)
	headerSortStyle := tcell.StyleDefault.Background(tcell.ColorNavy).Foreground(tcell.ColorYellow).Bold(true)
	activeHeaderStyle := tcell.StyleDefault.Background(tcell.ColorDarkCyan).Foreground(tcell.ColorWhite).Bold(true)
	activeHeaderSortStyle := tcell.StyleDefault.Background(tcell.ColorDarkCyan).Foreground(tcell.ColorYellow).Bold(true)
	activeLabelStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorAqua).Bold(true)
	dimStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorGray)
	dirStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorAqua).Bold(true)
	hiddenDirStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorTeal).Bold(true)
	hiddenFileStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorYellow)
	symlinkStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorFuchsia)
	selectedStyle := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack).Bold(true)
	selectedUnfocused := tcell.StyleDefault.Background(tcell.ColorDarkGray).Foreground(tcell.ColorWhite).Bold(true)
	hoverStyle := tcell.StyleDefault.Background(tcell.ColorDarkSlateGray).Foreground(tcell.ColorWhite)
	activePlaceStyle := tcell.StyleDefault.Background(tcell.ColorDarkBlue).Foreground(tcell.ColorWhite).Bold(true)
	inputStyle := tcell.StyleDefault.Background(tcell.ColorNavy).Foreground(tcell.ColorSilver)
	inputFocusStyle := tcell.StyleDefault.Background(tcell.ColorDarkCyan).Foreground(tcell.ColorWhite).Bold(true)
	arrowStyle := tcell.StyleDefault.Background(tcell.ColorDarkCyan).Foreground(tcell.ColorYellow).Bold(true)
	btnStyle := tcell.StyleDefault.Background(tcell.ColorDarkSlateGray).Foreground(tcell.ColorWhite)
	btnDimStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorGray)
	primaryBtnStyle := tcell.StyleDefault.Background(tcell.ColorDarkGreen).Foreground(tcell.ColorWhite).Bold(true)
	crumbStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorLightCyan)
	crumbSepStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorGray)
	errStyle := tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true)

	// 1. 배경 및 외곽 테두리
	for y := 0; y < boxH; y++ {
		for x := 0; x < boxW; x++ {
			st := bgStyle
			r := ' '
			if y == 0 || y == boxH-1 || x == 0 || x == boxW-1 {
				st = borderStyle
				switch {
				case x == 0 && y == 0:
					r = '┌'
				case x == boxW-1 && y == 0:
					r = '┐'
				case x == 0 && y == boxH-1:
					r = '└'
				case x == boxW-1 && y == boxH-1:
					r = '┘'
				case y == 0 || y == boxH-1:
					r = '─'
				default:
					r = '│'
				}
			}
			setCell(boxX+x, boxY+y, r, nil, st)
		}
	}

	// 구분선: 상단(row 2), 하단(row boxH-4), 좌우 수직 분할선(rows 3 .. boxH-5)
	sepTopY := boxY + 2
	sepBotY := boxY + boxH - 4
	for x := 1; x < boxW-1; x++ {
		setCell(boxX+x, sepTopY, '─', nil, borderStyle)
		setCell(boxX+x, sepBotY, '─', nil, borderStyle)
	}
	setCell(boxX, sepTopY, '├', nil, borderStyle)
	setCell(boxX+boxW-1, sepTopY, '┤', nil, borderStyle)
	setCell(boxX, sepBotY, '├', nil, borderStyle)
	setCell(boxX+boxW-1, sepBotY, '┤', nil, borderStyle)

	if fp.showPlaces {
		divSt := borderStyle
		if fp.focus == fpFocusPlaces || fp.focus == fpFocusList {
			divSt = activeDividerStyle
		}
		setCell(splitX, sepTopY, '┬', nil, divSt)
		setCell(splitX, sepBotY, '┴', nil, divSt)
		for y := boxY + 3; y < sepBotY; y++ {
			setCell(splitX, y, '│', nil, divSt)
		}
	}

	// 2. 타이틀 (row 0)
	title := T("fp.title.open")
	if fp.mode == fpModeSaveAs {
		title = T("fp.title.save_as")
	} else if fp.readOnly {
		title = T("fp.title.open_ro")
	}
	titleW := runewidth.StringWidth(title)
	tx := boxX + (boxW-titleW)/2
	if tx < boxX+2 {
		tx = boxX + 2
	}
	drawTextAt(setCell, tx, boxY, boxX+boxW-2, title, titleStyle)

	// 3. Dolphin 상단 툴바: [<] [>] [^] + 브레드크럼 경로 바 + [+ 폴더] [x] .* 숨김 (row 1)
	toolY := boxY + 1
	navX := boxX + 2

	backSt := btnDimStyle
	if fp.canGoBack() {
		backSt = btnStyle
	}
	fwdSt := btnDimStyle
	if fp.canGoForward() {
		fwdSt = btnStyle
	}
	upSt := btnDimStyle
	if filepath.Dir(fp.currentDir) != fp.currentDir {
		upSt = btnStyle
	}

	backEnd := drawTextAt(setCell, navX, toolY, boxX+boxW-2, "[<]", backSt)
	fp.backBtn = fpRect{X1: navX, X2: backEnd - 1, Y: toolY}
	fwdStart := backEnd + 1
	fwdEnd := drawTextAt(setCell, fwdStart, toolY, boxX+boxW-2, "[>]", fwdSt)
	fp.fwdBtn = fpRect{X1: fwdStart, X2: fwdEnd - 1, Y: toolY}
	upStart := fwdEnd + 1
	upEnd := drawTextAt(setCell, upStart, toolY, boxX+boxW-2, "[^]", upSt)
	fp.upBtn = fpRect{X1: upStart, X2: upEnd - 1, Y: toolY}

	// 우측 툴바 버튼: [+ 폴더] [x] .* 숨김
	hiddenLabel := T("fp.btn.hidden_on")
	if !fp.showHidden {
		hiddenLabel = T("fp.btn.hidden_off")
	}
	if boxW < 76 {
		hiddenLabel = T("fp.btn.hidden_short_on")
		if !fp.showHidden {
			hiddenLabel = T("fp.btn.hidden_short_off")
		}
	}
	newDirLabel := T("fp.btn.new_dir")
	hiddenW := runewidth.StringWidth(hiddenLabel)
	newDirW := runewidth.StringWidth(newDirLabel)

	hiddenX := boxX + boxW - 2 - hiddenW
	newDirX := hiddenX - 1 - newDirW
	drawTextAt(setCell, newDirX, toolY, newDirX+newDirW, newDirLabel, btnStyle)
	drawTextAt(setCell, hiddenX, toolY, hiddenX+hiddenW, hiddenLabel, bgStyle)
	fp.newDirBtn = fpRect{X1: newDirX, X2: newDirX + newDirW - 1, Y: toolY}
	fp.hiddenChk = fpRect{X1: hiddenX, X2: hiddenX + hiddenW - 1, Y: toolY}

	// 중앙 경로 바 (Dolphin 브레드크럼 또는 Ctrl+L 직접 입력)
	pathStart := upEnd + 1
	pathEnd := newDirX - 2
	fp.pathBox = fpRect{X1: pathStart, X2: pathEnd, Y: toolY}
	fp.crumbs = fp.crumbs[:0]

	if fp.focus == fpFocusPath {
		if pathEnd >= pathStart {
			cx := fp.pathInput.draw(setCell, pathStart, toolY, pathEnd-pathStart+1, inputFocusStyle, arrowStyle, true)
			fp.cursorX, fp.cursorY = cx, toolY
		}
	} else if pathEnd >= pathStart {
		availW := pathEnd - pathStart + 1
		parts := buildPathCrumbs(fp.currentDir)
		sepStr := " › "
		sepW := runewidth.StringWidth(sepStr)
		totalW := 0
		for i, p := range parts {
			totalW += runewidth.StringWidth(p.Label)
			if i > 0 {
				totalW += sepW
			}
		}
		startIdx := 0
		prefixEllipsis := false
		for totalW > availW && startIdx < len(parts)-1 {
			totalW -= runewidth.StringWidth(parts[startIdx].Label)
			if startIdx > 0 {
				totalW -= sepW
			}
			startIdx++
			prefixEllipsis = true
			if startIdx == 1 {
				totalW += 3 + sepW
			}
		}

		cx := pathStart
		if prefixEllipsis {
			cx = drawTextAt(setCell, cx, toolY, pathEnd+1, "..."+sepStr, crumbSepStyle)
		}
		for i := startIdx; i < len(parts); i++ {
			p := parts[i]
			if i > startIdx {
				cx = drawTextAt(setCell, cx, toolY, pathEnd+1, sepStr, crumbSepStyle)
			}
			label := p.Label
			rem := pathEnd - cx + 1
			if rem <= 0 {
				break
			}
			if runewidth.StringWidth(label) > rem {
				label = truncateRightByWidth(label, rem)
			}
			segStart := cx
			segStyle := crumbStyle
			if i == len(parts)-1 {
				segStyle = crumbStyle.Bold(true)
			}
			cx = drawTextAt(setCell, cx, toolY, pathEnd+1, label, segStyle)
			if cx > segStart {
				fp.crumbs = append(fp.crumbs, fpCrumb{
					Label: label,
					Path:  p.Path,
					Rect:  fpRect{X1: segStart, X2: cx - 1, Y: toolY},
				})
			}
		}
	}

	// 4. 패널 헤더 행 (row 3) — 포커스된 패널 헤더를 밝은 색(DarkCyan)으로 강조
	hdrY := boxY + 3
	placesFocused := fp.focus == fpFocusPlaces
	listFocused := fp.focus == fpFocusList

	if fp.showPlaces {
		pHdrSt := headerStyle
		placesHdr := " " + T("fp.places.header")
		if placesFocused {
			pHdrSt = activeHeaderStyle
		}
		for x := fp.placesX1; x <= fp.placesX2; x++ {
			setCell(x, hdrY, ' ', nil, pHdrSt)
		}
		drawTextAt(setCell, fp.placesX1, hdrY, fp.placesX2+1, placesHdr, pHdrSt)
	}

	// 우측 상세 보기(Details View) 컬럼 폭 계산
	visRows := fp.visibleListRows()
	showScrollbar := len(fp.entries) > visRows
	rightInnerW := fp.listX2 - fp.listX1 + 1
	contentW := rightInnerW
	fp.scrollbarX = -1
	if showScrollbar {
		contentW = rightInnerW - 1
		fp.scrollbarX = fp.listX2
	}

	showDateCol := contentW >= 50
	dateColW := 0
	if showDateCol {
		dateColW = 17
	}
	sizeColW := 11
	if contentW < 34 {
		sizeColW = 9
	}
	nameColW := contentW - sizeColW - dateColW
	if nameColW < 10 {
		nameColW = 10
	}

	nameColX := fp.listX1
	sizeColX := nameColX + nameColW
	dateColX := sizeColX + sizeColW

	listHdrBase := headerStyle
	listHdrSort := headerSortStyle
	if listFocused {
		listHdrBase = activeHeaderStyle
		listHdrSort = activeHeaderSortStyle
	}
	for x := fp.listX1; x <= fp.listX2; x++ {
		setCell(x, hdrY, ' ', nil, listHdrBase)
	}

	sortMark := func(col fpSortCol) string {
		if fp.sortCol != col {
			return ""
		}
		if fp.sortAsc {
			return " ▲"
		}
		return " ▼"
	}

	nameHdrSt := listHdrBase
	if fp.sortCol == fpSortName {
		nameHdrSt = listHdrSort
	}
	sizeHdrSt := listHdrBase
	if fp.sortCol == fpSortSize {
		sizeHdrSt = listHdrSort
	}
	dateHdrSt := listHdrBase
	if fp.sortCol == fpSortModTime {
		dateHdrSt = listHdrSort
	}

	drawTextAt(setCell, nameColX, hdrY, sizeColX, " "+T("fp.col.name")+sortMark(fpSortName), nameHdrSt)
	fp.colNameHdr = fpRect{X1: nameColX, X2: sizeColX - 1, Y: hdrY}

	sizeHdrMax := fp.listX1 + contentW
	if showDateCol {
		sizeHdrMax = dateColX
	}
	drawTextAt(setCell, sizeColX, hdrY, sizeHdrMax, T("fp.col.size")+sortMark(fpSortSize), sizeHdrSt)
	fp.colSizeHdr = fpRect{X1: sizeColX, X2: sizeHdrMax - 1, Y: hdrY}

	if showDateCol {
		drawTextAt(setCell, dateColX, hdrY, fp.listX1+contentW, T("fp.col.modified")+sortMark(fpSortModTime), dateHdrSt)
		fp.colDateHdr = fpRect{X1: dateColX, X2: fp.listX1 + contentW - 1, Y: hdrY}
	} else {
		fp.colDateHdr = fpRect{X1: 1, X2: 0, Y: -1}
	}

	// 스크롤 화살표 ▲ / ▼ (커맨드 팔레트와 동일하게 클릭으로 페이지 스크롤)
	fp.scrollUpBtn = fpRect{X1: 1, X2: 0, Y: -1}
	fp.scrollDownBtn = fpRect{X1: 1, X2: 0, Y: -1}
	if fp.scrollOff > 0 {
		ax := fp.listX2
		setCell(ax, hdrY, '▲', nil, listHdrBase.Foreground(tcell.ColorYellow).Bold(true))
		fp.scrollUpBtn = fpRect{X1: ax - 1, X2: ax, Y: hdrY}
	}
	if fp.scrollOff+visRows < len(fp.entries) {
		ax := fp.listX2
		setCell(ax, sepBotY, '▼', nil, borderStyle.Foreground(tcell.ColorYellow).Bold(true))
		fp.scrollDownBtn = fpRect{X1: ax - 1, X2: ax, Y: sepBotY}
	}

	// 5. 좌측 위치(Places) 사이드바 렌더링
	if fp.showPlaces {
		cleanCur := filepath.Clean(fp.currentDir)
		placesW := fp.placesX2 - fp.placesX1 + 1
		for row := 0; row < visRows; row++ {
			idx := fp.placesScrollOff + row
			y := fp.listY1 + row
			if idx >= len(fp.places) {
				break
			}
			pl := fp.places[idx]
			isActive := pl.Path == cleanCur
			isFocusedSel := fp.focus == fpFocusPlaces && idx == fp.placeCursor
			isHover := idx == fp.hoverPlaceIdx

			st := sidebarBgStyle
			if isFocusedSel {
				st = selectedStyle
			} else if isActive {
				st = activePlaceStyle
			} else if isHover {
				st = hoverStyle
			}
			for x := fp.placesX1; x <= fp.placesX2; x++ {
				setCell(x, y, ' ', nil, st)
			}
			prefix := " "
			lbl := truncateRightByWidth(prefix+strings.TrimSpace(pl.displayLabel()), placesW-1)
			drawTextAt(setCell, fp.placesX1, y, fp.placesX2+1, lbl, st)
		}
	}

	// 6. 우측 상세 파일 테이블 렌더링
	if len(fp.entries) == 0 {
		emptyMsg := T("fp.empty.filter")
		if fp.filterInput.text() == "" {
			emptyMsg = T("fp.empty.dir")
		}
		drawTextAt(setCell, fp.listX1+1, fp.listY1, fp.listX1+contentW, emptyMsg, dimStyle)
	} else {
		for row := 0; row < visRows; row++ {
			idx := fp.scrollOff + row
			if idx >= len(fp.entries) {
				break
			}
			ent := fp.entries[idx]
			y := fp.listY1 + row
			isSelected := idx == fp.cursor
			isHover := idx == fp.hoverListIdx && !isSelected

			rowStyle := bgStyle
			if isSelected {
				if fp.focus == fpFocusList || fp.focus == fpFocusFilter {
					rowStyle = selectedStyle
				} else {
					rowStyle = selectedUnfocused
				}
			} else if isHover {
				rowStyle = hoverStyle
			} else if ent.IsParent || ent.IsDir {
				if ent.IsHidden {
					rowStyle = hiddenDirStyle
				} else {
					rowStyle = dirStyle
				}
			} else if ent.IsSymlink {
				rowStyle = symlinkStyle
			} else if ent.IsHidden {
				rowStyle = hiddenFileStyle
			}

			for x := 0; x < contentW; x++ {
				setCell(fp.listX1+x, y, ' ', nil, rowStyle)
			}

			prefix := "  "
			displayName := ent.Name
			if ent.IsDir {
				displayName += "/"
			} else if ent.IsSymlink {
				displayName += "@"
			}
			maxNameW := nameColW - 3
			if maxNameW < 4 {
				maxNameW = 4
			}
			displayName = truncateRightByWidth(displayName, maxNameW)
			drawTextAt(setCell, nameColX, y, sizeColX-1, prefix+displayName, rowStyle)

			var sizeText string
			if ent.IsParent {
				sizeText = "<UP>"
			} else if ent.IsDir {
				if ent.IsSymlink {
					sizeText = "<LNK>"
				} else {
					sizeText = "<DIR>"
				}
			} else if ent.IsBrokenLink {
				sizeText = "<ERR>"
			} else {
				sizeText = formatFileSize(ent.Size)
			}
			metaSt := rowStyle
			if !isSelected && !isHover {
				metaSt = dimStyle
				if ent.IsDir {
					metaSt = dirStyle
				}
			}
			drawTextAt(setCell, sizeColX, y, sizeHdrMax-1, sizeText, metaSt)

			if showDateCol && !ent.IsParent && !ent.ModTime.IsZero() {
				dateStr := ent.ModTime.Format("2006-01-02 15:04")
				dateSt := rowStyle
				if !isSelected && !isHover {
					dateSt = dimStyle
				}
				drawTextAt(setCell, dateColX, y, fp.listX1+contentW, dateStr, dateSt)
			}
		}
	}

	// 우측 스크롤바 트랙
	if showScrollbar && visRows > 0 {
		sbX := fp.scrollbarX
		thumbH := visRows * visRows / len(fp.entries)
		if thumbH < 1 {
			thumbH = 1
		}
		maxOff := len(fp.entries) - visRows
		thumbStart := 0
		if maxOff > 0 {
			thumbStart = fp.scrollOff * (visRows - thumbH) / maxOff
		}
		for row := 0; row < visRows; row++ {
			ch := '│'
			st := dimStyle
			if row >= thumbStart && row < thumbStart+thumbH {
				ch = '█'
				st = tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorLightCyan)
			}
			setCell(sbX, fp.listY1+row, ch, nil, st)
		}
	}

	// 7. 하단 필터 바 + Dolphin 상태 요약 (row boxH-3)
	filterY := boxY + boxH - 3
	summary := Tf("fp.summary.full", fp.dirCount+fp.fileCount, fp.dirCount, fp.fileCount, formatFileSize(fp.totalFileBytes))
	summaryW := runewidth.StringWidth(summary)
	maxSummaryW := boxW / 2
	if summaryW > maxSummaryW {
		summary = Tf("fp.summary.short", fp.dirCount+fp.fileCount, fp.dirCount, fp.fileCount)
		summaryW = runewidth.StringWidth(summary)
	}
	summaryX := boxX + boxW - 1 - summaryW
	if summaryX < boxX+24 {
		summaryX = boxX + boxW - 1
	} else {
		drawTextAt(setCell, summaryX, filterY, boxX+boxW-1, summary, dimStyle)
	}

	filterFocused := fp.focus == fpFocusFilter
	filterLblSt := dimStyle
	filterPrefix := " " + T("fp.label.filter")
	if filterFocused {
		filterLblSt = activeLabelStyle
	}
	filterStart := drawTextAt(setCell, boxX+1, filterY, summaryX-1, filterPrefix, filterLblSt)
	filterEnd := summaryX - 2
	if filterEnd-filterStart > 32 {
		filterEnd = filterStart + 32
	}
	fp.filterBox = fpRect{X1: filterStart, X2: filterEnd, Y: filterY}
	if filterEnd >= filterStart {
		st := inputStyle
		if filterFocused {
			st = inputFocusStyle
		}
		cx := fp.filterInput.draw(setCell, filterStart, filterY, filterEnd-filterStart+1, st, arrowStyle, filterFocused)
		if filterFocused {
			fp.cursorX, fp.cursorY = cx, filterY
		}
	}

	// 8. 하단 파일명 입력창 & 액션 버튼 (row boxH-2)
	botY := boxY + boxH - 2
	cancelLabel := T("fp.btn.cancel")
	okLabel := T("fp.btn.open")
	if fp.focus == fpFocusNewDir {
		okLabel = T("fp.btn.create")
	} else if fp.mode == fpModeSaveAs {
		okLabel = T("fp.btn.save")
	}
	cancelW := runewidth.StringWidth(cancelLabel)
	okW := runewidth.StringWidth(okLabel)

	cancelX := boxX + boxW - 2 - cancelW
	okX := cancelX - 1 - okW
	drawTextAt(setCell, okX, botY, okX+okW, okLabel, primaryBtnStyle)
	drawTextAt(setCell, cancelX, botY, cancelX+cancelW, cancelLabel, btnStyle)
	fp.okBtn = fpRect{X1: okX, X2: okX + okW - 1, Y: botY}
	fp.cancelBtn = fpRect{X1: cancelX, X2: cancelX + cancelW - 1, Y: botY}

	botFocused := fp.focus == fpFocusFilename || fp.focus == fpFocusNewDir
	botLblSt := dimStyle
	botLabelKey := "fp.label.name"
	if fp.focus == fpFocusNewDir {
		botLabelKey = "fp.label.new_dir"
	}
	botPrefix := " " + T(botLabelKey)
	if botFocused {
		botLblSt = activeLabelStyle
	}
	inStart := drawTextAt(setCell, boxX+1, botY, okX-1, botPrefix, botLblSt)
	inEnd := okX - 2
	fp.bottomInputBox = fpRect{X1: inStart, X2: inEnd, Y: botY}
	if inEnd >= inStart {
		if fp.focus == fpFocusNewDir {
			cx := fp.newDirInput.draw(setCell, inStart, botY, inEnd-inStart+1, inputFocusStyle, arrowStyle, true)
			fp.cursorX, fp.cursorY = cx, botY
		} else {
			st := inputStyle
			if fp.focus == fpFocusFilename {
				st = inputFocusStyle
			}
			cx := fp.filenameInput.draw(setCell, inStart, botY, inEnd-inStart+1, st, arrowStyle, fp.focus == fpFocusFilename)
			if fp.focus == fpFocusFilename {
				fp.cursorX, fp.cursorY = cx, botY
			}
		}
	}

	// 9. 하단 테두리 힌트 또는 에러 메시지 (row boxH-1)
	if fp.errorMsg != "" {
		msg := " " + truncateRightByWidth(fp.errorMsg, boxW-4) + " "
		drawTextAt(setCell, boxX+2, boxY+boxH-1, boxX+boxW-2, msg, errStyle)
	} else {
		var hint string
		switch fp.focus {
		case fpFocusPath:
			hint = T("fp.hint.path")
		case fpFocusNewDir:
			hint = T("fp.hint.new_dir")
		default:
			hint = e.buildFilePickerDefaultHint()
		}
		hint = truncateRightByWidth(hint, boxW-4)
		hx := boxX + (boxW-runewidth.StringWidth(hint))/2
		if hx < boxX+2 {
			hx = boxX + 2
		}
		drawTextAt(setCell, hx, boxY+boxH-1, boxX+boxW-2, hint, borderStyle)
	}
}

func (e *Editor) buildFilePickerDefaultHint() string {
	var parts []string
	backKey := e.shortcutOf("fp_back")
	fwdKey := e.shortcutOf("fp_forward")
	if backKey != "" && fwdKey != "" {
		if strings.HasPrefix(backKey, "Alt+") && strings.HasPrefix(fwdKey, "Alt+") {
			parts = append(parts, backKey+"/"+strings.TrimPrefix(fwdKey, "Alt+")+":"+T("fp.hint.nav"))
		} else {
			parts = append(parts, backKey+"/"+fwdKey+":"+T("fp.hint.nav"))
		}
	} else if backKey != "" {
		parts = append(parts, backKey+":"+T("fp.hint.nav"))
	} else if fwdKey != "" {
		parts = append(parts, fwdKey+":"+T("fp.hint.nav"))
	}
	parts = append(parts, "Tab:"+T("fp.hint.pane"))
	if k := e.shortcutOf("fp_edit_path"); k != "" {
		parts = append(parts, k+":"+T("fp.hint.path_lbl"))
	}
	if k := e.shortcutOf("fp_new_dir"); k != "" {
		parts = append(parts, k+":"+T("fp.hint.new_dir_lbl"))
	}
	if k := e.shortcutOf("fp_toggle_hidden"); k != "" {
		parts = append(parts, k+":"+T("fp.hint.hidden_lbl"))
	}
	parts = append(parts, "Esc:"+T("fp.hint.cancel_lbl"))
	return " " + strings.Join(parts, "  ") + " "
}

func buildPathCrumbs(dir string) []fpCrumb {
	clean := filepath.Clean(dir)
	var raw []fpCrumb
	cur := clean
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			raw = append(raw, fpCrumb{Label: cur, Path: cur})
			break
		}
		raw = append(raw, fpCrumb{Label: filepath.Base(cur), Path: cur})
		cur = parent
	}
	for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}
	return raw
}

func (e *Editor) drawConfirmDialog(s tcell.Screen, w, h int) {
	cd := e.confirmDialog
	if cd == nil {
		return
	}

	setCell := func(x, y int, r rune, comb []rune, style tcell.Style) {
		if x >= 0 && x < w && y >= 0 && y < h {
			s.SetContent(x, y, r, comb, style)
		}
	}

	msgLines := strings.Split(cd.message, "\n")
	boxW := 48
	if tw := runewidth.StringWidth(cd.title) + 6; tw > boxW {
		boxW = tw
	}
	for _, line := range msgLines {
		if lw := runewidth.StringWidth(line) + 6; lw > boxW {
			boxW = lw
		}
	}
	if boxW > w-4 {
		boxW = w - 4
	}
	if boxW < 28 {
		boxW = 28
	}

	innerMax := boxW - 4
	var wrapped []string
	for _, line := range msgLines {
		if runewidth.StringWidth(line) <= innerMax {
			wrapped = append(wrapped, line)
			continue
		}
		var cur strings.Builder
		curW := 0
		for _, r := range line {
			rw := runewidth.RuneWidth(r)
			if curW+rw > innerMax && curW > 0 {
				wrapped = append(wrapped, cur.String())
				cur.Reset()
				curW = 0
			}
			cur.WriteRune(r)
			curW += rw
		}
		if cur.Len() > 0 {
			wrapped = append(wrapped, cur.String())
		}
	}

	boxH := len(wrapped) + 6
	if boxH > h-2 {
		boxH = h - 2
	}
	if boxH < 7 {
		boxH = 7
	}

	boxX := (w - boxW) / 2
	boxY := (h - boxH) / 2
	cd.boxX, cd.boxY, cd.boxW, cd.boxH = boxX, boxY, boxW, boxH

	clearDoubleWidthLeftEdge(s, setCell, boxX, boxY, boxH, w, h)

	borderStyle := tcell.StyleDefault.Background(tcell.ColorDarkRed).Foreground(tcell.ColorWhite).Bold(true)
	bgStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite)
	btnStyle := tcell.StyleDefault.Background(tcell.ColorDarkSlateGray).Foreground(tcell.ColorWhite)
	btnFocusStyle := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack).Bold(true)

	for y := 0; y < boxH; y++ {
		for x := 0; x < boxW; x++ {
			st := bgStyle
			r := ' '
			if y == 0 || y == boxH-1 || x == 0 || x == boxW-1 {
				st = borderStyle
				switch {
				case x == 0 && y == 0:
					r = '┌'
				case x == boxW-1 && y == 0:
					r = '┐'
				case x == 0 && y == boxH-1:
					r = '└'
				case x == boxW-1 && y == boxH-1:
					r = '┘'
				case y == 0 || y == boxH-1:
					r = '─'
				default:
					r = '│'
				}
			}
			setCell(boxX+x, boxY+y, r, nil, st)
		}
	}

	if cd.title != "" {
		t := truncateRightByWidth(cd.title, boxW-4)
		tx := boxX + (boxW-runewidth.StringWidth(t))/2
		drawTextAt(setCell, tx, boxY, boxX+boxW-2, t, borderStyle)
	}

	for i, line := range wrapped {
		ly := boxY + 2 + i
		if ly >= boxY+boxH-3 {
			break
		}
		lx := boxX + (boxW-runewidth.StringWidth(line))/2
		if lx < boxX+2 {
			lx = boxX + 2
		}
		drawTextAt(setCell, lx, ly, boxX+boxW-2, line, bgStyle)
	}

	yesTxt := " " + cd.yesLabel + " "
	noTxt := " " + cd.noLabel + " "
	yesW := runewidth.StringWidth(yesTxt)
	noW := runewidth.StringWidth(noTxt)
	gap := 4
	totalBtnW := yesW + gap + noW
	btnY := boxY + boxH - 2
	yesX := boxX + (boxW-totalBtnW)/2
	noX := yesX + yesW + gap

	yesSt := btnStyle
	noSt := btnStyle
	if cd.focusYes {
		yesSt = btnFocusStyle
	} else {
		noSt = btnFocusStyle
	}
	drawTextAt(setCell, yesX, btnY, yesX+yesW, yesTxt, yesSt)
	drawTextAt(setCell, noX, btnY, noX+noW, noTxt, noSt)
	cd.yesBtn = fpRect{X1: yesX, X2: yesX + yesW - 1, Y: btnY}
	cd.noBtn = fpRect{X1: noX, X2: noX + noW - 1, Y: btnY}
}
