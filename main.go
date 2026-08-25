package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"time"
	"unicode"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	"github.com/fsnotify/fsnotify"
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
	"github.com/ncruces/zenity"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// --- [ 1. 설정 및 도구 정의 ] ---
type Config struct {
	ShowLineNumbers bool   `json:"show_line_numbers"`
	LineWrapping    bool   `json:"line_wrapping"`
	TabSize         int    `json:"tab_size"`
	DateFormat      string `json:"date_format"`
	HighlightLine   bool   `json:"highlight_line"`
	OverlapSearch   bool   `json:"overlap_search"`
	AutoIndent      bool   `json:"auto_indent"`
	ExpandTab       bool   `json:"expand_tab"`
	SmartBackspace  bool   `json:"smart_backspace"`

	// 💡 액션 ID -> 단축키 문자열. 기본값과 다른 항목만 기록된다 (빈 문자열 = 바인딩 해제).
	// nil 이면 전부 기본값. 자세한 내용은 [ 단축키 바인딩 엔진 ] 섹션 참조.
	Keybindings map[string]string `json:"keybindings,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		ShowLineNumbers: true,
		LineWrapping:    true,
		TabSize:         4,
		DateFormat:      "%Y-%m-%d %H:%M:%S",
		HighlightLine:   true,
		OverlapSearch:   false,
		AutoIndent:      true,
		ExpandTab:       true,
		SmartBackspace:  false,
	}
}
func getConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "jigedit", "config.json")
}

func LoadConfig() Config {
	configPath := getConfigPath()
	if configPath == "" {
		return DefaultConfig()
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultCfg := DefaultConfig()
		_ = SaveConfig(defaultCfg)
		return defaultCfg
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return DefaultConfig()
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig()
	}
	// 💡 0 이하일 때 20000으로 강제하는 로직 삭제 (0을 제한 없음으로 인정)
	return cfg
}

// 💡 config.json 을 통째로 다시 쓴다. 단축키 메뉴처럼 에디터가 스스로 설정을
// 바꾸는 경로에서 사용한다. 저장 자체는 비원자적(직접 덮어쓰기)이며, 이는
// saveToFile 과 동일한 의도적 설계다.
func SaveConfig(cfg Config) error {
	configPath := getConfigPath()
	if configPath == "" {
		return fmt.Errorf("설정 경로를 찾을 수 없습니다")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

func (e *Editor) closeBuffer(idx int) {
	b := e.buffers[idx]
	if b.filePath != "" {
		e.fileWatcher.Remove(b.filePath)
	}
	// 🟢 타이머 좀비화 방지 코드가 안전하게 제거되었습니다.

	// 💡 슬라이스에서 제거할 때 끝부분의 포인터를 nil로 덮어써서 GC가 메모리를 회수하게 함
	copy(e.buffers[idx:], e.buffers[idx+1:])
	e.buffers[len(e.buffers)-1] = nil
	e.buffers = e.buffers[:len(e.buffers)-1]

	// 💡 탭을 다 닫아서 0개가 되면 에디터를 안전하게 종료합니다 (튕김 방지)
	if len(e.buffers) == 0 {
		shuttingDown.Store(true)
		if globalScreenHandle != nil && *globalScreenHandle != nil {
			(*globalScreenHandle).Fini()
		}
		os.Exit(0)
	}

	if e.activeBuffer == idx {
		if e.activeBuffer >= len(e.buffers) {
			e.activeBuffer = len(e.buffers) - 1
		}
	} else if e.activeBuffer > idx {
		e.activeBuffer--
	}
}

func convertLinuxDateToGoLayout(linuxFormat string) string {
	replacer := strings.NewReplacer(
		"%Y", "2006", "%y", "06", "%m", "01", "%d", "02",
		"%H", "15", "%I", "03", "%M", "04", "%S", "05",
		"%p", "PM", "%a", "Mon", "%A", "Monday", "%b", "Jan", "%B", "January",
	)
	return replacer.Replace(linuxFormat)
}

// 💡 이름으로 인코딩 객체를 매핑해주는 범용 엔진
type EncodingGroup struct {
	Region    string
	Encodings []string
}

var encodingGroups = []EncodingGroup{
	{Region: "Unicode", Encodings: []string{"UTF-8", "UTF-16 LE", "UTF-16 BE"}},
	{Region: "Korean", Encodings: []string{"UTF-8", "CP949 (EUC-KR)"}},
	{Region: "Japanese", Encodings: []string{"Shift-JIS", "EUC-JP", "ISO-2022-JP"}},
	{Region: "Chinese Simplified", Encodings: []string{"GBK", "GB18030", "HZ-GB2312"}},
	{Region: "Chinese Traditional", Encodings: []string{"Big5"}},
	{Region: "Western European", Encodings: []string{"ISO-8859-1", "ISO-8859-15", "CP1252"}},
	{Region: "Central European", Encodings: []string{"ISO-8859-2", "CP1250"}},
	{Region: "Cyrillic", Encodings: []string{"KOI8-R", "KOI8-U", "CP1251", "ISO-8859-5"}},
	{Region: "Greek", Encodings: []string{"ISO-8859-7", "CP1253"}},
	{Region: "Turkish", Encodings: []string{"ISO-8859-9", "CP1254"}},
	{Region: "Hebrew", Encodings: []string{"ISO-8859-8", "CP1255"}},
	{Region: "Arabic", Encodings: []string{"ISO-8859-6", "CP1256"}},
	{Region: "Thai", Encodings: []string{"CP874", "CP1258"}},
	{Region: "Baltic", Encodings: []string{"ISO-8859-4", "ISO-8859-13", "CP1257"}},
	{Region: "Nordic", Encodings: []string{"ISO-8859-10"}},
}

// 💡 [극한 최적화] 99%의 영문/숫자/기호는 룩업 테이블을 거치지 않고 0.000001초만에 통과시킵니다.
func fastRuneWidth(r rune, tabSize int) int {
	if r == '\t' {
		return tabSize
	}
	if r < 128 {
		return 1
	} // ASCII는 무조건 1칸! (속도 500% 향상)
	return runewidth.RuneWidth(r)
}

// 💡 이름으로 인코딩 객체를 매핑해주는 범용 엔진
func getTextEncoding(name string) encoding.Encoding {
	switch name {
	// Korean
	case "CP949 (EUC-KR)", "CP949", "EUC-KR":
		return korean.EUCKR

		// Unicode
	case "UTF-16 LE":
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM)
	case "UTF-16 BE":
		return xunicode.UTF16(xunicode.BigEndian, xunicode.UseBOM)

		// Japanese
	case "Shift-JIS":
		return japanese.ShiftJIS
	case "EUC-JP":
		return japanese.EUCJP
	case "ISO-2022-JP":
		return japanese.ISO2022JP

		// Chinese
	case "GBK":
		return simplifiedchinese.GBK
	case "GB18030":
		return simplifiedchinese.GB18030
	case "HZ-GB2312":
		return simplifiedchinese.HZGB2312
	case "Big5":
		return traditionalchinese.Big5

		// Western / Central European
	case "ISO-8859-1":
		return charmap.ISO8859_1
	case "ISO-8859-15":
		return charmap.ISO8859_15
	case "CP1252":
		return charmap.Windows1252
	case "ISO-8859-2":
		return charmap.ISO8859_2
	case "CP1250":
		return charmap.Windows1250

		// Cyrillic (Russian)
	case "KOI8-R":
		return charmap.KOI8R
	case "KOI8-U":
		return charmap.KOI8U
	case "CP1251":
		return charmap.Windows1251
	case "ISO-8859-5":
		return charmap.ISO8859_5

		// Greek & Turkish
	case "ISO-8859-7":
		return charmap.ISO8859_7
	case "CP1253":
		return charmap.Windows1253
	case "ISO-8859-9":
		return charmap.ISO8859_9
	case "CP1254":
		return charmap.Windows1254

		// Hebrew & Arabic
	case "ISO-8859-8":
		return charmap.ISO8859_8
	case "CP1255":
		return charmap.Windows1255
	case "ISO-8859-6":
		return charmap.ISO8859_6
	case "CP1256":
		return charmap.Windows1256

		// Thai, Baltic, Nordic
	case "CP874":
		return charmap.Windows874
	case "CP1258":
		return charmap.Windows1258
	case "ISO-8859-4":
		return charmap.ISO8859_4
	case "ISO-8859-13":
		return charmap.ISO8859_13
	case "CP1257":
		return charmap.Windows1257
	case "ISO-8859-10":
		return charmap.ISO8859_10

	default:
		return nil // UTF-8
	}
}

// 💡 KWrite(Uchardet) 수준의 통계학 기반 언어 감지 엔진 (모든 인코딩 완벽 매핑)

// --- [ 2. 자료구조 정의 ] ---

// 💡 메모리 할당이 전혀 없는 초고속 단일 라인 체크섬 함수
func fnvHash(b []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, v := range b {
		h ^= uint64(v)
		h *= 1099511628211
	}
	return h
}

// 💡 1. 절대 좌표 구조체 (이전의 Node 포인터를 완벽히 대체)
type Loc struct {
	L       int // Line (0부터 시작)
	C       int // Column (0부터 시작)
	TargetX int // ponytail: memory of target X for vertical movement
}

type Range struct {
	Start Loc
	End   Loc
}

// 화면 렌더링용 (더 이상 Node 포인터를 안 씀)
type VisualLine struct {
	isWrapped bool
	startCX   int
	endCX     int
	width     int
}

type MatchInfo struct {
	loc        Loc
	matchLen   int
	submatches []int
}

// 💡 2. Undo/Redo를 위한 초간단 Action 객체
type Action struct {
	IsInsert bool
	Start    Loc
	End      Loc
	Text     string
	// Anchor is where THIS action's caret sat right before the edit. For an
	// insert it's always Start, but for a delete it depends on direction
	// (backspace's anchor is End, forward-delete's is Start), which can't be
	// recovered from Start/End alone -- so it's captured at record time.
	Anchor Loc
	// IsPrimary marks whether this action belongs to the primary cursor
	// (b.cursor) as opposed to one of the extra multi-cursors. Like micro,
	// each edit is tagged to the specific caret that made it, rather than
	// undo/redo snapshotting the whole cursor set.
	IsPrimary bool
}

type Transaction struct {
	ID        int64
	Actions   []Action
	BeforeLoc Loc
	AfterLoc  Loc
	Time      time.Time
}

// 💡 3. 순수하게 텍스트(lines)와 좌표(cursor)만 가지는 완벽히 분리된 Buffer
type Buffer struct {
	lines       [][]byte
	vCache      map[int][]VisualLine // 💡 핵심: 줄바꿈 상태를 기억하는 캐시 맵 (메모리 최적화)
	cursor      Loc
	selection   Range
	isSelecting bool

	vOffsetL   int // 🟢 [변경] 현재 화면 상단의 물리 줄 번호 (0부터 시작)
	vOffsetSub int // 🟢 [변경] 현재 화면 상단의 물리 줄 내에서의 래핑 번호 (0부터 시작)
	hOffset    int
	isReadOnly bool

	filePath   string
	isConfig   bool
	isModified bool
	encoding   string

	searchMode   bool
	replaceStep  int
	isReplace    bool
	searchQuery  []rune
	replaceQuery []rune
	matches      []MatchInfo
	matchIdx     int
	searchCapped bool

	undoStack      []Transaction
	redoStack      []Transaction
	totalUndoBytes int
	currentTx      *Transaction
	txIDCounter    int64
	savedTxID      int64
	// nextActionIsExtra tells InsertTextWithRecord/DeleteTextWithRecord whether
	// the caller is currently recording an edit for an extra (non-primary)
	// cursor, so each Action can be tagged with the caret it belongs to. Zero
	// value is false, matching the single-cursor default.
	nextActionIsExtra bool

	gotoMode       bool
	gotoInput      []rune
	inputCX        int
	closeBtnStartX int
	closeBtnEndX   int
	inputSelStart  int
	inputSelEnd    int
	isInputSelect  bool
	inputHOffset   int

	searchRegex bool
	searchCase  bool
	searchWord  bool
	chkRegexX1  int
	chkRegexX2  int
	chkCaseX1   int
	chkCaseX2   int
	chkWordX1   int
	chkWordX2   int

	lastExternalSync time.Time

	encodeBtnX1 int
	encodeBtnX2 int

	// 렌더링 캐시
	vLinesValid    bool
	cachedMaxWidth int
	cachedConfig   Config
	totalChars     int

	// Net Change 감지용
	savedTotalChars int

	// 💡 [추가] O(1) Net Change 감지용 체크섬 필드
	currentHash     uint64
	savedHash       uint64
	savedStrongHash uint64

	stickToWrapEnd  bool
	dirtyStartL     int  // 🟢 [추가됨] 내용이 변경된 가장 윗줄 번호 기록
	endsWithNewline bool // 💡 파일 끝 개행 보존용 플래그
	savedModTime    time.Time
	savedSize       int64

	// ponytail: multi-cursor support
	extraCursors []Loc // additional cursor positions (primary stays in b.cursor)
	// extraSelAnchors holds each extra cursor's OWN selection anchor, index-
	// aligned with extraCursors (extraSelAnchors[i] is the anchor for
	// extraCursors[i]; that cursor's live position is its own selection's
	// other end -- mirrors how b.selection.Start/b.cursor work for the
	// primary). Only trusted when its length matches extraCursors; any
	// mismatch (e.g. a cursor was added/removed mid-selection) is treated as
	// "no extra selection" rather than read stale/misaligned data.
	extraSelAnchors []Loc
}
type PaletteItem struct {
	Name     string
	Shortcut string
	Action   EditorAction
}

type TabBound struct {
	Idx    int
	StartX int
	EndX   int
	Y      int
}

type Editor struct {
	buffers      []*Buffer
	activeBuffer int
	cfg          Config

	paletteActive bool
	paletteItems  []PaletteItem
	paletteCursor int
	promptMode    bool
	promptType    string
	alertMessage  string

	tabBounds []TabBound
	tabHeight int
	paletteX  int
	paletteY  int
	paletteW  int
	paletteH  int

	targetCloseBuffer   int
	externalChangeQueue []int
	targetEncoding      string // 💡 다시 열기 시 사용자가 선택한 인코딩 기억

	ctxMenuActive bool
	ctxMenuItems  []PaletteItem
	ctxMenuCursor int
	ctxMenuX      int
	ctxMenuY      int
	ctxMenuW      int
	ctxMenuH      int

	fileWatcher *fsnotify.Watcher

	prevCursor      Loc
	prevSelStart    Loc
	prevSelEnd      Loc
	prevTotalChars  int
	prevTxID        int64
	prevIsSelecting bool

	// 💡 추가됨: 인코딩 선택 메뉴 상태
	encodeMenuActive bool
	encodeMenuState  int    // 0:닫힘, 1:액션, 2:지역, 3:인코딩
	encodeMenuAction string // "save" or "reopen"
	encodeRegionIdx  int
	encodeMenuTitle  string
	encodeMenuItems  []PaletteItem
	encodeMenuCursor int
	encodeMenuX      int
	encodeMenuY      int
	encodeMenuW      int
	encodeMenuH      int

	// 💡 단축키 설정 메뉴 상태 (인코딩 메뉴와 동일한 구조)
	keyMenuActive     bool
	keyMenuState      int    // 0:닫힘, 1:목록, 2:새 키 캡처
	keyMenuTargetID   string // state 2 에서 재바인딩 중인 액션 ID
	keyMenuTitle      string
	keyMenuItems      []PaletteItem
	keyMenuCursor     int
	keyMenuListCursor int // 캡처 화면에 들어가기 직전의 목록 위치
	keyMenuListH      int // 캡처 화면에 들어가기 직전의 목록 실제 높이 (돌아올 때 스크롤 계산용)
	keyMenuX          int
	keyMenuY          int
	keyMenuW          int
	keyMenuH          int

	// 💡 라이브 단축키 표. cfg 가 바뀔 때마다 applyConfig 가 다시 만든다.
	bindings  map[KeyChord]*ActionDef
	bindingOf map[string]KeyChord

	needsFullRefresh bool
	mouseX           int
	mouseY           int
	menuScrollOffset int

	prevActiveBuf     int
	prevVOffset       int
	prevVOffsetSub    int // 🟢 [추가] 서브 래핑 줄 캐시 백업용
	prevHOffset       int
	prevPalette       bool
	prevCtxMenu       bool
	prevEncode        bool
	prevKeyMenu       bool
	prevPrompt        bool
	prevSearch        bool
	prevGoto          bool
	prevReplace       bool
	prevLinesLen      int
	initialBufferUsed bool

	// ponytail: previous frame's caret set, so multi-cursor mode only forces a
	// whole-screen repaint when the carets actually moved -- not on every
	// keystroke while extra cursors merely exist.
	prevExtraCursors    []Loc
	prevExtraSelAnchors []Loc
}

// 💡 메뉴 아이템 이름의 첫 글자가 입력한 알파벳과 일치하는 인덱스를 탐색하는 헬퍼 함수
func getMenuJumpIdx(items []PaletteItem, r rune, currentIdx int) int {
	target := unicode.ToLower(r)

	checkMatch := func(idx int) bool {
		name := strings.TrimSpace(items[idx].Name)
		name = strings.TrimPrefix(name, "<")
		name = strings.TrimPrefix(name, ">")
		name = strings.TrimSpace(name)
		if len(name) > 0 {
			firstRune, _ := utf8.DecodeRuneInString(name)
			return unicode.ToLower(firstRune) == target
		}
		return false
	}

	// 1. 현재 커서 '다음' 항목부터 리스트 끝까지 탐색
	for i := currentIdx + 1; i < len(items); i++ {
		if checkMatch(i) {
			return i
		}
	}

	// 2. 리스트 끝까지 없으면, 처음부터 현재 커서 위치까지 탐색 (Wrap-around)
	for i := 0; i <= currentIdx && i < len(items); i++ {
		if checkMatch(i) {
			return i
		}
	}

	return -1
}

func NewEditor() *Editor {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("파일 감지기를 초기화할 수 없습니다: %v", err)
	}

	e := &Editor{
		buffers:          []*Buffer{NewBuffer()},
		activeBuffer:     0,
		fileWatcher:      watcher,
		needsFullRefresh: true,
		prevActiveBuf:    -1,
		prevVOffset:      -1,
		prevHOffset:      -1,
	}
	e.applyConfig(LoadConfig())
	e.initEncodingMenu() // 💡 인코딩 메뉴 초기화 호출

	go e.listenFileChanges()

	return e
}

// 💡 설정을 적용하는 유일한 경로. cfg 를 그냥 대입하면 단축키 표와 메뉴의
// 표시 문자열이 낡은 채로 남으므로, e.cfg 를 바꾸는 곳은 전부 이 함수를 쓴다.
func (e *Editor) applyConfig(cfg Config) {
	e.cfg = cfg
	e.bindings, e.bindingOf = buildBindings(cfg)
	e.initPalette()
	e.initContextMenu()
	e.needsFullRefresh = true
}

// 💡 액션 ID 의 현재 단축키 표시 문자열. 바인딩이 없으면 빈 문자열.
func (e *Editor) shortcutOf(id string) string {
	return chordString(e.bindingOf[id])
}

// 💡 액션 ID 로 팔레트/컨텍스트 메뉴 항목을 만든다. Shortcut 이 라이브 바인딩에서
// 파생되므로 리바인딩 후에도 표시가 어긋나지 않는다.
func (e *Editor) actionItem(id string) PaletteItem {
	a, ok := actionByID[id]
	if !ok {
		return PaletteItem{Name: id}
	}
	return PaletteItem{Name: a.Name, Shortcut: e.shortcutOf(id), Action: a.Fn}
}

func (e *Editor) listenFileChanges() {
	timers := make(map[string]*time.Timer)
	var mu sync.Mutex
	defer func() {
		mu.Lock()
		for _, t := range timers {
			t.Stop()
		}
		mu.Unlock()
	}()
	for {
		select {
		case event, ok := <-e.fileWatcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				name := event.Name
				mu.Lock()
				if t, exists := timers[name]; exists {
					t.Reset(300 * time.Millisecond)
				} else {
					timers[name] = time.AfterFunc(300*time.Millisecond, func() {
						if shuttingDown.Load() {
							return
						}
						if globalScreenHandle != nil && *globalScreenHandle != nil {
							(*globalScreenHandle).PostEvent(tcell.NewEventInterrupt(name))
						}
						mu.Lock()
						delete(timers, name)
						mu.Unlock()
					})
				}
				mu.Unlock()
			}
		case err, ok := <-e.fileWatcher.Errors:
			if !ok {
				return
			}
			log.Println("watcher error:", err)
		}
	}
}

func (e *Editor) initContextMenu() {
	e.ctxMenuItems = []PaletteItem{
		e.actionItem("copy"),
		e.actionItem("cut"),
		e.actionItem("paste"),
		e.actionItem("select_all"),
		e.actionItem("close_tab"),
	}
}

// 💡 추가됨: 인코딩 메뉴 아이템 세팅

// 🟢 지운 자리에 아래의 두 함수를 통째로 붙여넣으세요.
// 💡 재사용과 경고창 연계를 위해 다시 열기 로직을 분리한 헬퍼 함수

func (e *Editor) initEncodingMenu() {} // 에러 방지용 빈 함수

func (e *Editor) showEncodeActionMenu(x, y int) {
	e.encodeMenuActive = true
	e.encodeMenuState = 1
	e.encodeMenuTitle = " Select Action "
	e.encodeMenuX, e.encodeMenuY = x, y
	e.encodeMenuW, e.encodeMenuH = 0, 0
	e.encodeMenuCursor = 0
	e.encodeMenuItems = []PaletteItem{
		{"저장될 인코딩 변경 (Set Save Encoding)...", "", func(e *Editor, s tcell.Screen) {
			e.encodeMenuAction = "save"
			e.showEncodeRegionMenu()
		}},
		{"다시 열기 (Reopen With...)", "", func(e *Editor, s tcell.Screen) {
			e.encodeMenuAction = "reopen"
			e.showEncodeRegionMenu()
		}},
	}
}

func (e *Editor) showEncodeRegionMenu() {
	e.encodeMenuActive = true
	e.encodeMenuState = 2
	e.encodeMenuTitle = " Select Region "
	e.encodeMenuW, e.encodeMenuH = 0, 0
	e.encodeMenuCursor = 0
	e.encodeMenuItems = []PaletteItem{
		{"< 뒤로 가기 (Back)", "", func(e *Editor, s tcell.Screen) { e.showEncodeActionMenu(e.encodeMenuX, e.encodeMenuY) }},
	}
	for i, group := range encodingGroups {
		idx := i
		e.encodeMenuItems = append(e.encodeMenuItems, PaletteItem{
			Name:   group.Region + " >",
			Action: func(e *Editor, s tcell.Screen) { e.encodeRegionIdx = idx; e.showEncodeEncodingMenu() },
		})
	}
}

func (e *Editor) showEncodeEncodingMenu() {
	e.encodeMenuActive = true
	e.encodeMenuState = 3
	group := encodingGroups[e.encodeRegionIdx]
	e.encodeMenuTitle = " " + group.Region + " "
	e.encodeMenuW, e.encodeMenuH = 0, 0
	e.encodeMenuCursor = 0
	e.encodeMenuItems = []PaletteItem{
		{"< 뒤로 가기 (Back)", "", func(e *Editor, s tcell.Screen) { e.showEncodeRegionMenu() }},
	}
	for _, enc := range group.Encodings {
		encName := enc
		e.encodeMenuItems = append(e.encodeMenuItems, PaletteItem{
			Name: encName,
			Action: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				if e.encodeMenuAction == "save" {
					if b.encoding != encName {
						b.encoding = encName
						b.isModified = true
					}
				} else if e.encodeMenuAction == "reopen" {
					if b.filePath == "" {
						return
					}
					if b.isModified {
						e.promptMode = true
						e.promptType = "reopen"
						e.targetEncoding = encName
						return
					}
					b.reopenWithEncoding(encName)
				}
			},
		})
	}
}

func (e *Editor) initPalette() {
	e.paletteItems = []PaletteItem{
		e.actionItem("open"),
		e.actionItem("save"),
		e.actionItem("save_as"),
		e.actionItem("new_tab"),
		e.actionItem("close_tab"),
		e.actionItem("next_tab"),
		e.actionItem("prev_tab"),
		e.actionItem("undo"),
		e.actionItem("redo"),
		e.actionItem("select_all"),
		e.actionItem("copy"),
		e.actionItem("cut"),
		e.actionItem("paste"),
		e.actionItem("find"),
		e.actionItem("replace"),
		e.actionItem("goto_line"),
		e.actionItem("insert_time"),

		e.actionItem("toggle_config"),

		{"단축키 설정 (Keybindings)", "", func(e *Editor, s tcell.Screen) { e.showKeybindMenu() }},

		{"설정 초기화 (Reset Config)", "", func(e *Editor, s tcell.Screen) {
			e.promptMode = true
			e.promptType = "reset_config"
		}},

		{"읽기 전용 모드 전환 (Toggle ReadOnly)", "", func(e *Editor, s tcell.Screen) {
			b := e.getActive()
			b.isReadOnly = !b.isReadOnly // 상태 반전
			e.needsFullRefresh = true    // 화면 UI(자물쇠 아이콘 등) 즉시 갱신
		}},

		e.actionItem("quit"),
	}
}

// 💡 단축키 설정 메뉴 (state 1: 전체 목록).
// 인코딩 메뉴와 같은 규칙: W/H 를 0 으로 리셋해 drawMenu 가 위치를 다시 계산하게 한다.
// menuScrollOffset 은 세 메뉴가 공유하는 필드라 반드시 같이 리셋해야 한다.
func (e *Editor) showKeybindMenu() {
	e.keyMenuActive = true
	e.keyMenuState = 1
	e.keyMenuTargetID = ""
	e.keyMenuTitle = " 단축키 설정 (Keybindings) "
	e.keyMenuW, e.keyMenuH = 0, 0
	e.keyMenuCursor = 0
	e.menuScrollOffset = 0

	e.keyMenuItems = []PaletteItem{
		{"모든 단축키 기본값으로 되돌리기 (Reset All)", "", func(e *Editor, s tcell.Screen) {
			e.promptMode = true
			e.promptType = "reset_keybinds"
		}},
	}
	for i := range Actions {
		a := &Actions[i]
		label := chordString(e.bindingOf[a.ID])
		if label == "" {
			label = "(없음)"
		}
		id := a.ID
		e.keyMenuItems = append(e.keyMenuItems, PaletteItem{
			Name:     a.Name,
			Shortcut: label,
			Action:   func(e *Editor, s tcell.Screen) { e.showKeybindCapture(id) },
		})
	}
	e.needsFullRefresh = true
}

// 💡 state 2: 새 키를 기다리는 캡처 화면. 항목은 안내문 두 줄뿐이고 Action 은 nil 이다
// (drawMenu 를 그대로 재사용하기 위한 표시용 항목).
func (e *Editor) showKeybindCapture(id string) {
	a, ok := actionByID[id]
	if !ok {
		return
	}
	// 💡 목록 화면의 실제 높이를 기억해 둔다. 아래에서 keyMenuW/H 를 0 으로
	// 리셋하면 캡처 화면 자체의 크기(항목 2개)로 재계산되어 버려서, 돌아갈 때
	// 쓸 "목록 기준 높이"를 여기서 미리 붙잡아 두지 않으면 잃어버린다.
	if e.keyMenuH > 0 {
		e.keyMenuListH = e.keyMenuH
	}
	e.keyMenuActive = true
	e.keyMenuState = 2
	e.keyMenuTargetID = id
	e.keyMenuListCursor = e.keyMenuCursor
	e.keyMenuTitle = " " + a.Name + " "
	e.keyMenuW, e.keyMenuH = 0, 0
	e.keyMenuCursor = -1
	e.menuScrollOffset = 0
	e.keyMenuItems = []PaletteItem{
		{Name: "새 단축키를 누르세요", Shortcut: chordString(e.bindingOf[id])},
		{Name: "Esc:취소  Delete:기본값  Backspace:해제"},
	}
	e.needsFullRefresh = true
}

// 💡 캡처 화면에서 목록으로 돌아온다. 26개짜리 목록이라 매번 맨 위로 튀면
// 아래쪽 항목을 연달아 고칠 때 괴롭다 — 보고 있던 위치를 복원한다.
func (e *Editor) backToKeybindList() {
	want := e.keyMenuListCursor
	savedH := e.keyMenuListH
	e.showKeybindMenu()
	if savedH > 0 {
		// 💡 showKeybindMenu() 가 keyMenuH 를 0 으로 리셋해 놓아서, 이 시점에 바로
		// followKeybindCursor 를 부르면 (다음 draw 가 재계산하기 전까지) 높이를
		// 0으로 여기고 스크롤 보정을 건너뛴다. 캡처에 들어가기 전 실제 높이를
		// 임시로 되돌려 놓아 이번 프레임부터 바로 정확히 스크롤되게 한다.
		e.keyMenuH = savedH
	}
	if want > 0 && want < len(e.keyMenuItems) {
		e.keyMenuCursor = want
	}
	e.followKeybindCursor()
}

// 💡 선택 항목이 보이도록 스크롤 창을 맞춘다. menuScrollOffset 은 모든 메뉴가
// 공유하는 필드이므로 단축키 메뉴가 열려 있을 때만 만진다.
func (e *Editor) followKeybindCursor() {
	visibleItems := e.keyMenuH - 2
	if visibleItems <= 0 || e.keyMenuCursor < 0 {
		return
	}
	if e.keyMenuCursor < e.menuScrollOffset {
		e.menuScrollOffset = e.keyMenuCursor
	} else if e.keyMenuCursor >= e.menuScrollOffset+visibleItems {
		e.menuScrollOffset = e.keyMenuCursor - visibleItems + 1
	}
}

func (e *Editor) closeKeybindMenu() {
	if e.keyMenuActive {
		e.needsFullRefresh = true
	}
	e.keyMenuActive = false
	e.keyMenuState = 0
	e.keyMenuTargetID = ""
	e.menuScrollOffset = 0 // 세 메뉴가 공유하는 필드라 나갈 때 비워 둔다
}

// 💡 단축키 하나를 확정한다. chord 가 제로면 해제.
// 성공하면 config.json 까지 즉시 기록하고 목록 화면으로 돌아간다.
// 실패하면 기존 alert 프롬프트로 이유를 알리고 캡처 화면에 머문다.
func (e *Editor) commitKeybind(id string, chord KeyChord) {
	a, ok := actionByID[id]
	if !ok {
		return
	}
	chord = normalizeChord(chord)

	if chord.isZero() {
		if id == actionIDPalette {
			e.showKeybindAlert("커맨드 팔레트는 해제할 수 없습니다 (되돌릴 방법이 사라집니다)")
			return
		}
	} else {
		if reservedChord(chord) {
			e.showKeybindAlert(chordString(chord) + " 는 편집기 예약 키라 사용할 수 없습니다")
			return
		}
		if other, dup := e.bindings[chord]; dup && other.ID != id {
			e.showKeybindAlert(chordString(chord) + " 는 이미 '" + other.Name + "' 에 할당되어 있습니다")
			return
		}
	}

	cfg := e.cfg
	byID := make(map[string]KeyChord, len(e.bindingOf))
	for k, v := range e.bindingOf {
		byID[k] = v
	}
	byID[a.ID] = chord
	cfg.Keybindings = keybindOverrides(byID)

	e.applyConfig(cfg)
	if err := SaveConfig(e.cfg); err != nil {
		e.showKeybindAlert("설정 저장 실패: " + err.Error())
	}
	e.syncConfigBuffers()
	e.backToKeybindList()
}

// 💡 에러는 기존 alert 프롬프트를 재사용한다. promptMode 는 이벤트 캐스케이드
// 최상단이라, Enter/Esc 로 닫으면 그 아래 살아 있는 단축키 메뉴로 자연히 돌아온다.
func (e *Editor) showKeybindAlert(msg string) {
	e.alertMessage = msg
	e.promptMode = true
	e.promptType = "alert"
	e.needsFullRefresh = true
}

// 💡 우리가 config.json 을 직접 덮어썼을 때, 열려 있는 설정 탭을 즉시 맞춰 준다.
// 수정 중인 탭은 건드리지 않고 파일 감시기의 external_change 프롬프트에 맡긴다.
func (e *Editor) syncConfigBuffers() {
	for _, buf := range e.buffers {
		if buf != nil && buf.isConfig && !buf.isModified {
			buf.reloadFromDisk()
		}
	}
}

// 🟢 여기에 아래 코드를 붙여넣으세요.

// --- [ 3. 코어 텍스트 엔진 (micro 아키텍처) ] ---

// 💡 좌표가 범위를 벗어나지 않도록 강제 보정하는 헬퍼 함수
func (b *Buffer) clampLoc(loc Loc) Loc {
	if loc.L < 0 {
		loc.L = 0
	}
	if loc.L >= len(b.lines) {
		loc.L = len(b.lines) - 1
	}
	if loc.C < 0 {
		loc.C = 0
	}
	if loc.C > len(b.lines[loc.L]) {
		loc.C = len(b.lines[loc.L])
	}
	return loc
}

func (b *Buffer) alignToRuneBoundary(loc Loc) Loc {
	loc = b.clampLoc(loc)
	line := b.lines[loc.L]
	if len(line) == 0 || loc.C == 0 || loc.C == len(line) {
		return loc
	}
	for loc.C > 0 && loc.C < len(line) && (line[loc.C]&0xC0) == 0x80 {
		loc.C--
	}
	return loc
}

func (b *Buffer) getContent() string {
	var sb strings.Builder
	totalBytes := 0
	for _, line := range b.lines {
		totalBytes += len(line)
	}
	sb.Grow(totalBytes + len(b.lines))
	for i, line := range b.lines {
		sb.Write(line)
		if i < len(b.lines)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func NewBuffer() *Buffer {
	emptyHash := fnvHash([]byte{})
	b := &Buffer{
		lines:           [][]byte{{}},
		vCache:          make(map[int][]VisualLine), // 💡 초기화
		cursor:          Loc{L: 0, C: 0, TargetX: -1},
		encoding:        "UTF-8",
		dirtyStartL:     -1,        // 🟢 [추가됨] -1은 변경 사항 없음(Clean)을 의미
		currentHash:     emptyHash, // 💡 초기화
		savedHash:       emptyHash, // 💡 초기화
		endsWithNewline: true,      // 💡 새 파일은 기본적으로 개행 종료로 가정
	}
	b.savedTotalChars = b.totalChars
	b.savedStrongHash = b.computeStrongHash()
	return b
}

// 💡 메모리를 단 1바이트도 쓰지 않는(Zero-Allocation) 극한의 원본 비교 엔진
// 💡 [극한 최적화] 무의미한 130MB 전체 메모리 비교를 삭제하고, 초고속 O(1) 트랜잭션 비교로 갱신 성능을 극대화합니다.
func (b *Buffer) checkModified() {
	var currentTxID int64 = 0
	if len(b.undoStack) > 0 {
		currentTxID = b.undoStack[len(b.undoStack)-1].ID
	}

	// 1. 트랜잭션 ID가 일치하면 무조건 Clean (Undo/Redo 지원)
	if currentTxID == b.savedTxID {
		b.isModified = false
		return
	}

	// 2. 빠른 부정: XOR 또는 글자수가 다르면 확실히 수정됨 (O(1))
	if b.currentHash != b.savedHash || b.totalChars != b.savedTotalChars {
		b.isModified = true
		return
	}

	// 3. 의심 구간(재배열 가능성): 순서 민감 강해시로 확정 (O(N), 희소)
	b.isModified = b.computeStrongHash() != b.savedStrongHash
}

func (b *Buffer) updateSavedFileInfo() {
	if b.filePath == "" {
		return
	}
	if info, err := os.Stat(b.filePath); err == nil {
		b.savedModTime = info.ModTime()
		b.savedSize = info.Size()
	}
}

func (b *Buffer) computeStrongHash() uint64 {
	var h uint64 = 14695981039346656037
	mix := func(p []byte) {
		for _, c := range p {
			h ^= uint64(c)
			h *= 1099511628211
		}
	}
	for i, line := range b.lines {
		mix(line)
		if i < len(b.lines)-1 {
			h ^= uint64('\n')
			h *= 1099511628211
		}
	}
	if b.endsWithNewline && len(b.lines) > 0 {
		h ^= uint64('\n')
		h *= 1099511628211
	}
	return h
}

func (b *Buffer) markSaved() {
	if len(b.undoStack) == 0 {
		b.savedTxID = 0
	} else {
		b.savedTxID = b.undoStack[len(b.undoStack)-1].ID
	}
	b.savedTotalChars = b.totalChars // 💡 저장 당시 글자 수 기록
	b.savedHash = b.currentHash      // 💡 저장 시점의 체크섬 낙인 점찍기
	b.savedStrongHash = b.computeStrongHash()
	b.isModified = false
	b.updateSavedFileInfo()
}

// ponytail: 원래 통계적 인코딩 감지(chardet)가 포함되어 무거운 하이브리드 엔진이 작동하던 자리였으나,
// 오작동을 방지하고 코드 복잡도를 최소화하기 위해 기본값을 UTF-8로 지정하고 BOM 패턴만 판단하도록 단순화했습니다.
// 수동으로 인코딩을 변경하여 다시 여는 기능은 그대로 유지됩니다.

// 💡 대용량 파일용 스트리밍 라인 로더 (Peak 메모리 최소화)
func loadFileLines(path string, forcedEncoding string) (lines [][]byte, encoding string, totalChars int, hash uint64, endsWithNewline bool, err error) {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, "", 0, 0, false, fmt.Errorf("일반 파일이 아닙니다")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, "", 0, 0, false, err
	}
	defer f.Close()

	// 1. 인코딩 감지용 샘플링 (최대 8KB)
	sample := make([]byte, 8192)
	n, err := f.Read(sample)
	if err != nil && err != io.EOF {
		return nil, "", 0, 0, false, err
	}
	sample = sample[:n]

	// 파일 포인터 초기화
	_, err = f.Seek(0, 0)
	if err != nil {
		return nil, "", 0, 0, false, err
	}

	detectedEnc := "UTF-8"
	if len(sample) > 0 {
		if bytes.HasPrefix(sample, []byte{0xEF, 0xBB, 0xBF}) {
			detectedEnc = "UTF-8"
			_, _ = f.Seek(3, 0)
		} else if bytes.HasPrefix(sample, []byte{0xFF, 0xFE}) {
			detectedEnc = "UTF-16 LE"
			_, _ = f.Seek(2, 0)
		} else if bytes.HasPrefix(sample, []byte{0xFE, 0xFF}) {
			detectedEnc = "UTF-16 BE"
			_, _ = f.Seek(2, 0)
		} else {
			detectedEnc = "UTF-8"
		}
	}

	// 강제 인코딩 지정이 있는 경우 덮어쓰기
	if forcedEncoding != "" {
		detectedEnc = forcedEncoding
	}

	var r io.Reader = f
	if detectedEnc != "UTF-8" {
		enc := getTextEncoding(detectedEnc)
		if enc != nil {
			r = transform.NewReader(f, enc.NewDecoder())
		}
	}

	// 2. bufio 스트리밍 라인 스캔 및 XOR 해시 계산
	reader := bufio.NewReader(r)
	hash = fnvHash([]byte{})
	lines = make([][]byte, 0, 1024)
	endsWithNewline = false

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				endsWithNewline = true
				line = line[:len(line)-1]
			} else {
				endsWithNewline = false
			}
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}

			// Go GC 메모리 압박 완화를 위해 필요시 슬라이스 용량 축소
			if cap(line) > len(line)+8 {
				trimmed := make([]byte, len(line))
				copy(trimmed, line)
				line = trimmed
			}

			totalChars += utf8.RuneCount(line)
			hash ^= fnvHash(line)
			lines = append(lines, line)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", 0, 0, false, err
		}
	}

	if len(lines) == 0 {
		lines = [][]byte{{}}
	}

	totalChars += len(lines) - 1

	return lines, detectedEnc, totalChars, hash, endsWithNewline, nil
}

// 💡 대용량 파일용 스트리밍 라이터 (Peak 메모리 최소화)
func (b *Buffer) saveToFile(path string) error {
	// 💡 인코딩 변환 무결성 사전 검증 (파일 파괴 방지)
	if b.encoding != "" && b.encoding != "UTF-8" {
		enc := getTextEncoding(b.encoding)
		if enc != nil {
			validator := transform.NewWriter(io.Discard, enc.NewEncoder())
			for i, line := range b.lines {
				if _, err := validator.Write(line); err != nil {
					return fmt.Errorf("인코딩 변환 실패 (저장 중단됨): %v", err)
				}
				if i < len(b.lines)-1 {
					if _, err := validator.Write([]byte{'\n'}); err != nil {
						return fmt.Errorf("인코딩 변환 실패 (저장 중단됨): %v", err)
					}
				}
			}
			if err := validator.Close(); err != nil {
				return fmt.Errorf("인코딩 변환 실패 (저장 중단됨): %v", err)
			}
		}
	}

	var perm os.FileMode = 0644
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()

	var w io.Writer = f
	if b.encoding != "" && b.encoding != "UTF-8" {
		enc := getTextEncoding(b.encoding)
		if enc != nil {
			w = transform.NewWriter(f, enc.NewEncoder())
		}
	}

	bufWriter := bufio.NewWriterSize(w, 1<<20)
	for i, line := range b.lines {
		_, err = bufWriter.Write(line)
		if err != nil {
			return err
		}
		if i < len(b.lines)-1 {
			_, err = bufWriter.Write([]byte{'\n'})
			if err != nil {
				return err
			}
		}
	}
	if b.endsWithNewline && len(b.lines) > 0 {
		_, err = bufWriter.Write([]byte{'\n'})
		if err != nil {
			return err
		}
	}
	return bufWriter.Flush()
}

func (b *Buffer) reloadFromDisk() bool {
	if b.filePath == "" || b.isModified {
		return false
	}

	lines, encoding, totalChars, hash, endsWithNewline, err := loadFileLines(b.filePath, b.encoding)
	if err != nil {
		return false
	}

	if hash == b.savedHash && totalChars == b.savedTotalChars {
		return false
	}

	b.reloadFromLines(lines, encoding, totalChars, hash, endsWithNewline)
	debug.FreeOSMemory() // 💡 로딩 직후 OS에 즉각 메모리 반환
	return true
}

func (b *Buffer) reloadFromLines(lines [][]byte, encoding string, totalChars int, hash uint64, endsWithNewline bool) {
	wasReadOnly := b.isReadOnly
	b.isReadOnly = false // 💡 일시적으로 읽기 전용 해제하여 외부 덮어쓰기 허용

	oldLines := b.lines
	b.BeginTransaction()
	lastL := len(oldLines) - 1
	lastC := 0
	if lastL >= 0 {
		lastC = len(oldLines[lastL])
	}
	b.DeleteTextWithRecord(Loc{L: 0, C: 0, TargetX: -1}, Loc{L: lastL, C: lastC, TargetX: -1})

	newText := string(bytes.Join(lines, []byte{'\n'}))
	b.InsertTextWithRecord(Loc{L: 0, C: 0, TargetX: -1}, newText)
	b.EndTransaction()

	b.isReadOnly = wasReadOnly // 💡 원래 상태로 복구

	b.encoding = encoding
	b.vCache = make(map[int][]VisualLine)
	b.vLinesValid = false
	b.totalChars = totalChars
	b.savedTotalChars = totalChars
	b.currentHash = hash
	b.savedHash = hash
	b.endsWithNewline = endsWithNewline
	b.savedStrongHash = b.computeStrongHash()
	b.lastExternalSync = time.Now()
	b.cursor = b.clampLoc(b.cursor)
	// 💡 외부 리로드로 줄 수가 줄어들 수 있으므로 보조 커서도 함께 클램프해야 한다.
	// 놓치면 다음 이동키가 b.lines[사라진 줄]에 접근해 패닉한다.
	// 앵커는 리로드 이전 내용을 가리키므로 폐기한다.
	b.extraSelAnchors = nil
	for i := range b.extraCursors {
		b.extraCursors[i] = b.clampLoc(b.extraCursors[i])
	}
	b.cleanExtraCursors()
	// 💡 UX 개선: 외부 리로드 시 undoStack을 초기화하지 않고 단일 트랜잭션으로 보존
	b.isSelecting = false
	b.isModified = false // 💡 디스크 내용과 동일하므로 수정 상태 해제
	b.savedTxID = b.txIDCounter
	b.updateSavedFileInfo()
}

func (b *Buffer) reopenWithEncoding(encName string) {
	lines, encoding, totalChars, hash, endsWithNewline, err := loadFileLines(b.filePath, encName)
	if err != nil {
		return
	}

	b.lines = lines
	b.encoding = encoding
	b.vCache = make(map[int][]VisualLine)
	b.vLinesValid = false
	b.totalChars = totalChars
	b.savedTotalChars = totalChars
	b.currentHash = hash
	b.savedHash = hash
	b.endsWithNewline = endsWithNewline
	b.savedStrongHash = b.computeStrongHash()
	b.isModified = false
	b.lastExternalSync = time.Now()
	b.cursor = Loc{L: 0, C: 0, TargetX: -1}
	b.clearExtraCursors() // 💡 새 내용에 대해 옛 보조 커서 좌표는 무효 — 방치하면 이동키가 패닉한다
	b.vOffsetL = 0
	b.vOffsetSub = 0
	b.hOffset = 0
	b.undoStack = nil
	b.redoStack = nil
	b.totalUndoBytes = 0
	b.isSelecting = false
	b.txIDCounter = 0
	b.savedTxID = 0
	b.updateSavedFileInfo()
}

func (b *Buffer) Insert(loc Loc, text string) Loc {
	loc = b.clampLoc(loc)
	//🟢 [추가됨] 수정된 가장 윗줄(도미노 시작점) 마킹
	if b.dirtyStartL == -1 || loc.L < b.dirtyStartL {
		b.dirtyStartL = loc.L
	}
	textBytes := []byte(text)
	if len(textBytes) == 0 {
		return loc
	}

	b.totalChars += utf8.RuneCount(textBytes)

	// 🟢 [Phase 1-1] 빠른 경로: 개행 없는 단일 라인 삽입 — 재할당/머리복사 제거
	if bytes.IndexByte(textBytes, '\n') < 0 {
		b.currentHash ^= fnvHash(b.lines[loc.L]) // 삽입 전 원본 해시 제거
		b.lines[loc.L] = slices.Insert(b.lines[loc.L], loc.C, textBytes...)
		b.currentHash ^= fnvHash(b.lines[loc.L]) // 삽입 후 신 해시 주입
		delete(b.vCache, loc.L)
		return Loc{L: loc.L, C: loc.C + len(textBytes), TargetX: -1}
	}

	// 다중라인 경로: 이하 코드는 \n 포함 텝스트에서만 실행됨
	// 해시 증분: 조작 전 원본 행의 해시를 전체 합에서 제거
	b.currentHash ^= fnvHash(b.lines[loc.L])

	var newLines [][]byte
	start := 0
	for i, c := range textBytes {
		if c == '\n' {
			newLines = append(newLines, append([]byte(nil), textBytes[start:i]...))
			start = i + 1
		}
	}
	newLines = append(newLines, append([]byte(nil), textBytes[start:]...))

	b.vCache = make(map[int][]VisualLine) // 💡 줄 바꿈 추가가 수반되므로 전체 캐시 무효화

	originalLine := b.lines[loc.L]
	tail := append([]byte(nil), originalLine[loc.C:]...)
	firstLine := make([]byte, 0, loc.C+len(newLines[0]))
	firstLine = append(firstLine, originalLine[:loc.C]...)
	firstLine = append(firstLine, newLines[0]...)

	b.lines[loc.L] = firstLine

	// 💡 [해시 증분] 쪼개진 첫 번째 줄의 새 해시 주입
	b.currentHash ^= fnvHash(b.lines[loc.L])

	newLines[len(newLines)-1] = append(newLines[len(newLines)-1], tail...)

	// 💡 [해시 증분] 새롭게 추가되는 모든 중간/끝 줄들의 해시를 주입
	for i := 1; i < len(newLines); i++ {
		b.currentHash ^= fnvHash(newLines[i])
	}

	// 💡 최적화: O(N) 임시 슬라이스 할당(Double Copy)을 방지하는 In-place Shift
	addLen := len(newLines) - 1
	b.lines = append(b.lines, make([][]byte, addLen)...)

	copy(b.lines[loc.L+addLen+1:], b.lines[loc.L+1:])
	copy(b.lines[loc.L+1:], newLines[1:])

	return Loc{L: loc.L + len(newLines) - 1, C: len(newLines[len(newLines)-1]) - len(tail), TargetX: -1}
}

func (b *Buffer) Remove(start, end Loc) string {
	start = b.clampLoc(start)
	end = b.clampLoc(end)
	if start.L > end.L || (start.L == end.L && start.C > end.C) {
		start, end = end, start
	}
	if b.dirtyStartL == -1 || start.L < b.dirtyStartL {
		b.dirtyStartL = start.L
	}
	if start.L == end.L && start.C == end.C {
		return ""
	}

	if start.L == end.L {
		line := b.lines[start.L]
		deleted := string(line[start.C:end.C]) // 🟢 [Phase 1-2] slices.Delete가 backing 덮기 전에 복사 — 순서 고정
		b.currentHash ^= fnvHash(line)         // 삭제 전 원본 해시 제거
		b.lines[start.L] = slices.Delete(line, start.C, end.C)
		b.totalChars -= utf8.RuneCountInString(deleted)
		b.currentHash ^= fnvHash(b.lines[start.L]) // 삭제 후 신 해시 주입
		delete(b.vCache, start.L)
		return deleted
	}

	b.vCache = make(map[int][]VisualLine) // 💡 줄 수가 변동되므로 전체 캐시 무효화

	// 💡 [해시 증분] 다중 행 삭제 시작. 범위 내에 걸쳐있는 모든 행의 기존 해시를 통째로 제거
	for i := start.L; i <= end.L; i++ {
		b.currentHash ^= fnvHash(b.lines[i])
	}

	var deletedBytes []byte
	lineStart := b.lines[start.L]
	lineEnd := b.lines[end.L]

	// 💡 최적화: 용량을 미리 계산하여 삭제 시 불필요한 배열 재할당 방지
	delCap := len(lineStart) - start.C + 1
	for i := start.L + 1; i < end.L; i++ {
		delCap += len(b.lines[i]) + 1
	}
	delCap += end.C
	deletedBytes = make([]byte, 0, delCap)

	deletedBytes = append(deletedBytes, lineStart[start.C:]...)
	deletedBytes = append(deletedBytes, '\n')
	for i := start.L + 1; i < end.L; i++ {
		deletedBytes = append(deletedBytes, b.lines[i]...)
		deletedBytes = append(deletedBytes, '\n')
	}
	deletedBytes = append(deletedBytes, lineEnd[:end.C]...)

	newLine := make([]byte, 0, start.C+len(lineEnd)-end.C)
	newLine = append(newLine, lineStart[:start.C]...)
	newLine = append(newLine, lineEnd[end.C:]...)
	b.lines[start.L] = newLine

	// 💡 [해시 증분] 두 줄이 병합되어 살아남은 첫 줄의 새 해시 주입
	b.currentHash ^= fnvHash(b.lines[start.L])

	// 💡 최적화: 임시 배열 생성 없이 제자리에서 잘라내기 병합 (단일 복사)
	oldLen := len(b.lines)
	b.lines = append(b.lines[:start.L+1], b.lines[end.L+1:]...)

	// 💡 메모리 누수 방지: 슬라이스 축소 후 꼬리 부분에 남은 포인터들을 nil로 초기화 (GC 수거 지원)
	for i := len(b.lines); i < oldLen; i++ {
		b.lines[:cap(b.lines)][i] = nil
	}

	deletedStr := string(deletedBytes)
	b.totalChars -= utf8.RuneCountInString(deletedStr)
	return deletedStr
}

// --- [ 4. 코어 논리 엔진 (Undo/Redo, Selection, Movement, VisualLine) ] ---

// actionOverheadBytes approximates the fixed in-memory cost of one Action
// struct (three Locs + a string header + two bools, rounded up for padding),
// on top of its Text payload. Without this, a multi-cursor edit that emits one
// Action per caret but very little text (e.g. N cursors each typing one char)
// looks nearly free to the undo-memory cap while actually costing N times the
// struct overhead.
const actionOverheadBytes = 96

func txBytes(tx Transaction) int {
	sz := 0
	for _, a := range tx.Actions {
		sz += len(a.Text) + actionOverheadBytes
	}
	return sz
}

// 💡 1. 절대 꼬이지 않는 초간단 Undo / Redo 시스템
func (b *Buffer) BeginTransaction() {
	if b.currentTx == nil {
		b.currentTx = &Transaction{
			BeforeLoc: b.cursor,
		}
	}
}

// mergeAction folds curr into *last following the single-cursor Undo/Redo merge
// rules — word-wise insert, backspace deletion, and forward-delete deletion.
// It returns whether the merge happened and how many text bytes were added.
// Multi-cursor merging reuses this verbatim by applying it to each caret's
// action pair, so a single cursor is simply the n == 1 case.
func mergeAction(last *Action, curr Action) (ok bool, addedBytes int) {
	if strings.Contains(curr.Text, "\n") || strings.Contains(last.Text, "\n") {
		return false, 0
	}

	// 1. 연속 삽입(Insert) 묶기 (단어 단위로 — 공백에서 끊김)
	if curr.IsInsert && last.IsInsert {
		isSpaceCurr := curr.Text == " " || curr.Text == "\t"
		isSpaceLast := strings.HasSuffix(last.Text, " ") || strings.HasSuffix(last.Text, "\t")
		if !isSpaceCurr && !isSpaceLast && curr.Start == last.End {
			last.Text += curr.Text
			last.End = curr.End
			return true, len(curr.Text)
		}
		return false, 0
	}

	// 2. 연속 삭제(Backspace / Delete) 묶기
	if !curr.IsInsert && !last.IsInsert {
		// 2-1. 백스페이스 방향 (현재 지운 범위의 끝이, 이전에 지운 범위의 시작점과 맞닿을 때)
		if curr.End == last.Start {
			last.Text = curr.Text + last.Text
			last.Start = curr.Start
			return true, len(curr.Text)
		}
		// 2-2. Delete 키 방향 (현재 지운 범위의 시작이, 이전에 지운 범위의 시작점과 같을 때)
		if curr.Start == last.Start {
			last.Text = last.Text + curr.Text
			last.End = Loc{L: last.Start.L, C: last.Start.C + len(last.Text), TargetX: -1}
			return true, len(curr.Text)
		}
	}

	return false, 0
}

// mergePairable reports whether two transactions' action lists may be merged
// index-by-index with mergeAction.
//
// mergeAction's adjacency tests (curr.Start == last.End, curr.End == last.Start)
// assume a single caret whose coordinates were never disturbed between the two
// transactions. That assumption breaks the moment two carets share a LINE: the
// lower caret's edit shifts the upper caret's byte offsets, so a forward-delete
// can accidentally satisfy the backspace branch and the two transactions fold
// into one whose replay corrupts the buffer on undo.
//
// It stays true when every action sits on its own line. mergeAction already
// rejects any action whose Text contains a newline, so line-local edits can't
// shift any other line's offsets: each pair is then exactly the single-caret
// case, and undo's reverse-index replay order stops mattering. n == 1 is the
// trivial instance of the same rule.
func mergePairable(last, curr []Action) bool {
	if len(last) != len(curr) || len(last) == 0 {
		return false
	}
	for i := range last {
		// paired actions must describe the same line...
		if last[i].Start.L != curr[i].Start.L {
			return false
		}
		// ...and no line may host two carets, in either transaction.
		for j := 0; j < i; j++ {
			if last[j].Start.L == last[i].Start.L || curr[j].Start.L == curr[i].Start.L {
				return false
			}
		}
	}
	return true
}

// indentOrDedent implements the Tab / Shift+Tab key. Extracted from the event
// loop verbatim so it can be exercised directly by tests; dedent is true for
// Shift+Tab and Backtab.
//
// Two shapes live here: a point-caret path that is cursor-count-agnostic, and a
// selection path. The selection path is primary-only as far as the SELECTION
// goes (Buffer.selection is singular), but extra carets are still live and must
// be rebased by the same per-line indent delta -- Loc.C is a byte offset, so a
// caret left un-rebased ends up mid-rune and the next keystroke splits a
// character.
func (b *Buffer) indentOrDedent(cfg Config, dedent bool) {
	b.BeginTransaction()
	hasSel := b.HasSelection()

	if !hasSel {
		// ponytail: cursor-count-agnostic point-caret path --
		// degenerates to the old single-cursor Tab/Shift+Tab when
		// there are no extra cursors. Selections still take the
		// separate branch below since only the primary can carry
		// one (Buffer.selection is singular, primary-only).
		if dedent {
			b.runMultiCursorDedent(cfg.TabSize)
		} else {
			var indentStr string
			if cfg.ExpandTab {
				indentStr = strings.Repeat(" ", cfg.TabSize)
			} else {
				indentStr = "\t"
			}
			b.runMultiCursorInsert(func(Loc, bool) string { return indentStr })
		}
	} else {
		s, e := b.getSelectionRange()
		if !hasSel {
			s = b.cursor
			e = b.cursor
		}
		oldCursor := b.cursor

		if dedent {
			for r := s.L; r <= e.L; r++ {
				line := b.lines[r]
				if len(line) > 0 {
					removeCount := 0
					if line[0] == '\t' {
						removeCount = 1
					} else {
						for removeCount < len(line) && removeCount < cfg.TabSize && line[removeCount] == ' ' {
							removeCount++
						}
					}
					if removeCount > 0 {
						b.DeleteTextWithRecord(Loc{L: r, C: 0, TargetX: -1}, Loc{L: r, C: removeCount, TargetX: -1})
						if r == oldCursor.L {
							oldCursor.C -= removeCount
							if oldCursor.C < 0 {
								oldCursor.C = 0
							}
						}
						// 💡 선택 영역은 프라이머리 전용이지만 보조 커서는 여기서도
						// 살아 있다. 같이 밀어주지 않으면 남은 오프셋이 룬 경계를
						// 벗어나 다음 입력이 글자를 쪼갠다(mojibake).
						for i := range b.extraCursors {
							if b.extraCursors[i].L == r {
								b.extraCursors[i].C -= removeCount
								if b.extraCursors[i].C < 0 {
									b.extraCursors[i].C = 0
								}
							}
						}
						if hasSel {
							if r == s.L {
								s.C -= removeCount
								if s.C < 0 {
									s.C = 0
								}
							}
							if r == e.L {
								e.C -= removeCount
								if e.C < 0 {
									e.C = 0
								}
							}
						}
					}
				}
			}
		} else {
			// 🟢 expand_tab 설정 상태에 따라 들여쓰기 텍스트를 다르게 빌드합니다.
			var indentStr string
			if cfg.ExpandTab {
				indentStr = strings.Repeat(" ", cfg.TabSize)
			} else {
				indentStr = "\t"
			}
			// Loc.C는 룬 인덱스가 아니라 바이트 오프셋이므로 바이트 길이로 센다.
			// (스페이스/탭은 1바이트라 값은 같지만 단위가 맞아야 한다.)
			indentLen := len(indentStr)

			// 선택 영역이 있으면 한 줄이든 여러 줄이든 항상 그 줄(들) 전체를
			// 들여쓰기한다 (Shift+Tab의 내어쓰기 루프와 대칭 — 아래 dedent
			// 브랜치는 s.L != e.L 여부를 따지지 않고 항상 줄 단위로 동작한다).
			// 예전에는 한 줄 안의 선택만 있으면 선택 텍스트를 지우고 탭 문자로
			// 바꿔치기했는데, 그러면 사용자가 줄 하나만 선택했을 때와 여러 줄을
			// 선택했을 때 Tab이 서로 다르게 동작해 혼란스러웠다.
			for r := e.L; r >= s.L; r-- {
				b.InsertTextWithRecord(Loc{L: r, C: 0, TargetX: -1}, indentStr)
				if r == oldCursor.L {
					oldCursor.C += indentLen
				}
				// 💡 보조 커서도 같은 줄이면 함께 밀어야 한다 (위 dedent와 동일한 이유)
				for i := range b.extraCursors {
					if b.extraCursors[i].L == r {
						b.extraCursors[i].C += indentLen
					}
				}
				if r == s.L {
					s.C += indentLen
				}
				if r == e.L {
					e.C += indentLen
				}
			}
		}

		b.cursor = b.alignToRuneBoundary(oldCursor)
		b.cleanExtraCursors() // 위에서 extraCursors를 직접 건드렸으므로 정리 필수
		if hasSel {
			b.selection.Start = b.alignToRuneBoundary(s)
			b.selection.End = b.alignToRuneBoundary(e)
		}
	}
	b.EndTransaction()
}

// replaceAllMatches applies the current replace template to every entry in
// b.matches, working from the last match backwards so an earlier match's edit
// never invalidates a later one's offsets, and leaves the caret where it was.
//
// Caret tracking reuses the multi-cursor coordinate helpers rather than doing
// its own arithmetic: Loc.C is a BYTE offset (a rune count lands mid-character
// on non-ASCII replacements) and preprocessReplaceTemplate can turn a literal
// \n in the replace field into a real newline, which shifts L as well as C.
func (b *Buffer) replaceAllMatches() {
	templateStr, templateBytes := preprocessReplaceTemplate(b.replaceQuery)
	re := b.getSearchRegex()
	b.BeginTransaction()
	cursor := b.cursor
	for i := len(b.matches) - 1; i >= 0; i-- {
		m := b.matches[i]
		replaceStr := templateStr
		if re != nil && len(m.submatches) > 0 {
			lineData := b.lines[m.loc.L]
			expanded := re.Expand(nil, templateBytes, lineData, m.submatches)
			replaceStr = string(expanded)
		}
		delEnd := Loc{L: m.loc.L, C: m.loc.C + m.matchLen, TargetX: -1}
		b.DeleteTextWithRecord(m.loc, delEnd)
		cursor = shiftLocForDelete(cursor, m.loc, delEnd)
		b.InsertTextWithRecord(m.loc, replaceStr)
		insEnd := b.cursor // InsertTextWithRecord leaves b.cursor at endLoc
		cursor = shiftLocForInsert(cursor, m.loc, insEnd)
		b.rebaseCaretsForEdit(m.loc, delEnd, m.loc, insEnd)
	}
	b.cursor = b.alignToRuneBoundary(b.clampLoc(cursor))
	b.cleanExtraCursors()
	b.EndTransaction()
}

func (b *Buffer) EndTransaction() {
	if b.currentTx != nil && len(b.currentTx.Actions) > 0 {
		b.txIDCounter++
		b.currentTx.ID = b.txIDCounter
		b.currentTx.AfterLoc = b.cursor
		b.currentTx.Time = time.Now()

		// 💡 스마트 Undo/Redo 병합(Merge) — 단일 커서 규칙(mergeAction)을 커서 수만큼
		// 짝지어 적용한다. 모든 커서의 액션이 병합 가능할 때만 트랜잭션을 병합하므로
		// 단일 커서(n == 1)는 이 로직의 특수한 경우가 된다.
		if len(b.undoStack) > 0 {
			lastTx := &b.undoStack[len(b.undoStack)-1]
			n := len(b.currentTx.Actions)
			// 방금 저장된 상태(savedTxID)의 트랜잭션이면 병합을 거부한다.
			// mergePairable: 같은 줄에 커서가 둘 이상이면 좌표 기준이 어긋나므로 병합 금지.
			if n == len(lastTx.Actions) &&
				time.Since(lastTx.Time) < 1*time.Second &&
				lastTx.ID != b.savedTxID &&
				mergePairable(lastTx.Actions, b.currentTx.Actions) {

				// 일부 커서만 병합되어 상태가 깨지는 일을 막기 위해 복사본에서 먼저 시도하고,
				// 모든 액션이 병합될 때만 실제 트랜잭션에 반영한다.
				trial := append([]Action(nil), lastTx.Actions...)
				merged, added := true, 0
				for i := 0; i < n; i++ {
					ok, delta := mergeAction(&trial[i], b.currentTx.Actions[i])
					if !ok {
						merged = false
						break
					}
					added += delta
				}
				if merged {
					lastTx.Actions = trial
					lastTx.AfterLoc = b.cursor
					lastTx.Time = b.currentTx.Time
					b.redoStack = nil
					b.currentTx = nil
					b.checkModified()
					b.totalUndoBytes += added
					return
				}
			}
		}

		b.undoStack = append(b.undoStack, *b.currentTx)
		b.totalUndoBytes += txBytes(*b.currentTx)
		b.redoStack = nil

		// Limit undo memory usage (max 256MB)
		const maxUndoBytes = 256 << 20
		evicted := false
		for len(b.undoStack) > 1 && b.totalUndoBytes > maxUndoBytes {
			b.totalUndoBytes -= txBytes(b.undoStack[0])
			b.undoStack = b.undoStack[1:]
			evicted = true
		}

		// 💡 메모리 누수 방지: 단순 슬라이싱은 기존 배열이 메모리에 남게 되므로,
		// 완전히 새로운 슬라이스로 복사(Copy)하여 가비지 컬렉터(GC)가 예전 메모리를 회수하게 함.
		if len(b.undoStack) > 1000 {
			newStack := make([]Transaction, 0, 800)
			newStack = append(newStack, b.undoStack[len(b.undoStack)-800:]...)
			b.undoStack = newStack
			b.totalUndoBytes = 0
			for _, tx := range b.undoStack {
				b.totalUndoBytes += txBytes(tx)
			}
		} else if evicted {
			newStack := make([]Transaction, len(b.undoStack))
			copy(newStack, b.undoStack)
			b.undoStack = newStack
		}
		b.checkModified()
	}
	b.currentTx = nil
}
func (b *Buffer) InsertTextWithRecord(loc Loc, text string) {
	if b.isReadOnly || text == "" {
		return
	}
	endLoc := b.Insert(loc, text)
	if b.currentTx != nil {
		b.currentTx.Actions = append(b.currentTx.Actions, Action{
			IsInsert: true, Start: loc, End: endLoc, Text: text,
			Anchor: loc, IsPrimary: !b.nextActionIsExtra,
		})
	}
	b.cursor = endLoc
}
func (b *Buffer) DeleteTextWithRecord(start, end Loc) {
	if b.isReadOnly {
		return
	}
	anchor := b.cursor // caret position for this specific caret right before the delete
	text := b.Remove(start, end)
	if text != "" && b.currentTx != nil {
		b.currentTx.Actions = append(b.currentTx.Actions, Action{
			IsInsert: false, Start: start, End: end, Text: text,
			Anchor: anchor, IsPrimary: !b.nextActionIsExtra,
		})
	}
	b.cursor = start
}

// DeleteSelection deletes the primary selection and, if any extra cursors
// carry their own independent selection (see extraSelAnchors), each of
// those too -- in one bottom-to-top pass so deleting a lower one never
// invalidates a not-yet-processed higher one's byte offsets.
func (b *Buffer) DeleteSelection() bool {
	if b.isReadOnly {
		return false
	}
	hasPrimary := b.HasSelection()
	// keyed by position only: TargetX is goal-column bookkeeping and must never
	// take part in identifying a caret (see sameCaret).
	caretKey := func(l Loc) Loc { return Loc{L: l.L, C: l.C, TargetX: -1} }
	anchorFor := map[Loc]Loc{}
	if len(b.extraSelAnchors) == len(b.extraCursors) {
		for i, ec := range b.extraCursors {
			// sameCaret, not !=: an anchor is snapshotted with TargetX == -1, so a
			// Shift+Up/Shift+Down round trip returns the caret to the same L/C with
			// a TargetX set. A full-struct compare reads that as a live selection,
			// and DeleteSelection then reports "handled" without deleting anything --
			// every caller guards on `if !b.DeleteSelection()`, so the keystroke is
			// swallowed.
			if !sameCaret(b.extraSelAnchors[i], ec) {
				anchorFor[caretKey(ec)] = b.extraSelAnchors[i]
			}
		}
	}

	if len(anchorFor) == 0 {
		// No extra cursor has a selection of its own: exactly the original
		// single-selection path, unchanged, so behavior without this feature
		// in play is byte-for-byte identical to before it existed.
		if !hasPrimary {
			return false
		}
		start, end := b.getSelectionRange()
		start = b.clampLoc(start) // 🟢 [추가] 삭제 시 좌표 이중 보정
		end = b.clampLoc(end)     // 🟢 [추가] 삭제 시 좌표 이중 보정
		if sameCaret(start, end) {
			b.clearSelection()
			return false
		}
		b.DeleteTextWithRecord(start, end)
		for i := range b.extraCursors {
			b.extraCursors[i] = shiftLocForDelete(b.extraCursors[i], start, end)
		}
		// 💡 필수: shiftLocForDelete가 삭제 범위 안에 있던 커서들을 모두 start로 모으므로
		// 여러 보조 커서가 프라이머리와 같은 자리에 겹칠 수 있다. 정리하지 않으면
		// 다음 입력이 같은 위치에 두세 번 삽입된다.
		b.cleanExtraCursors()
		b.clearSelection()
		return true
	}

	var primaryStart, primaryEnd Loc
	if hasPrimary {
		primaryStart, primaryEnd = b.getSelectionRange()
		primaryStart = b.clampLoc(primaryStart)
		primaryEnd = b.clampLoc(primaryEnd)
	}
	b.runMultiCursorDelete(func(loc Loc, isPrimary bool) (Loc, Loc) {
		if isPrimary {
			if hasPrimary && !sameCaret(primaryStart, primaryEnd) {
				return primaryStart, primaryEnd
			}
			return loc, loc // primary has no selection: no-op for this caret
		}
		if anchor, ok := anchorFor[caretKey(loc)]; ok {
			return normalizeRange(anchor, loc)
		}
		return loc, loc // this extra caret has no selection: no-op
	})
	b.clearSelection()
	return true
}
func (b *Buffer) Undo() {
	if b.isReadOnly || len(b.undoStack) == 0 {
		return
	}
	tx := b.undoStack[len(b.undoStack)-1]
	b.undoStack = b.undoStack[:len(b.undoStack)-1]
	b.totalUndoBytes -= txBytes(tx)
	for i := len(tx.Actions) - 1; i >= 0; i-- {
		act := tx.Actions[i]
		if act.IsInsert {
			b.Remove(act.Start, act.End)
		} else {
			b.Insert(act.Start, act.Text)
		}
	}
	b.cursor = b.clampLoc(tx.BeforeLoc)
	b.restoreExtraCursorsFromActions(tx.Actions, undoCaretFor)
	b.redoStack = append(b.redoStack, tx)
	b.checkModified()
	b.clearSelection()
}
func (b *Buffer) Redo() {
	if b.isReadOnly || len(b.redoStack) == 0 {
		return
	}
	tx := b.redoStack[len(b.redoStack)-1]
	b.redoStack = b.redoStack[:len(b.redoStack)-1]
	for _, act := range tx.Actions {
		if act.IsInsert {
			b.Insert(act.Start, act.Text)
		} else {
			b.Remove(act.Start, act.End)
		}
	}
	b.cursor = b.clampLoc(tx.AfterLoc)
	b.restoreExtraCursorsFromActions(tx.Actions, redoCaretFor)
	b.undoStack = append(b.undoStack, tx)
	b.totalUndoBytes += txBytes(tx)
	b.checkModified()
	b.clearSelection()
}

// undoCaretFor returns where a specific caret should land after reversing act.
func undoCaretFor(act Action) Loc {
	if act.IsInsert {
		return act.Start
	}
	return act.Anchor
}

// redoCaretFor returns where a specific caret should land after re-applying act.
func redoCaretFor(act Action) Loc {
	if act.IsInsert {
		return act.End
	}
	return act.Start
}

// restoreExtraCursorsFromActions repositions the currently-live extra cursors
// using this transaction's own non-primary actions -- there is no separate
// snapshot of "the cursor set" to fall back on, matching micro, where each
// edit is tagged to the caret that made it rather than the whole cursor set
// being captured at undo boundaries.
//
// If the live extra-cursor count no longer matches the number of non-primary
// actions (e.g. the user dismissed multi-cursor mode with Escape, or added/
// removed cursors since this edit), the mapping from actions to cursors is no
// longer meaningful, so the live set is left untouched instead of resurrecting
// stale carets or clobbering unrelated ones.
func (b *Buffer) restoreExtraCursorsFromActions(actions []Action, caretFor func(Action) Loc) {
	var extras []Loc
	for _, act := range actions {
		if !act.IsPrimary {
			extras = append(extras, caretFor(act))
		}
	}
	if len(extras) == 0 || len(extras) != len(b.extraCursors) {
		// Can't confidently map this transaction's actions to the live
		// cursors, so leave their POSITIONS alone rather than resurrecting
		// dismissed carets or clobbering unrelated ones. But the undo/redo
		// that just ran may have changed the buffer's shape (removed lines,
		// shortened a line), and cleanExtraCursors clamps each one back onto
		// valid bounds -- skipping this left stale carets pointing past the
		// end of the buffer, silently failing to render ("disappearing"),
		// only resurfacing at a clamped-but-unintended spot the next time an
		// edit ran them through Insert/Remove's own clampLoc.
		b.cleanExtraCursors()
		return
	}
	b.extraCursors = extras
	b.cleanExtraCursors()
}

// 💡 2. 절대 좌표 기반 선택(Selection) 조작 로직
func (b *Buffer) getSelectionRange() (Loc, Loc) {
	s, e := b.selection.Start, b.selection.End
	if s.L > e.L || (s.L == e.L && s.C > e.C) {
		return e, s
	}
	return s, e
}
func (b *Buffer) HasSelection() bool {
	s, e := b.selection.Start, b.selection.End
	return s.L != e.L || s.C != e.C
}
func (b *Buffer) isLocInSelection(loc Loc) bool {
	if !b.HasSelection() {
		return false
	}
	s, e := b.getSelectionRange()
	if loc.L < s.L || loc.L > e.L {
		return false
	}
	if loc.L == s.L && loc.C < s.C {
		return false
	}
	if loc.L == e.L && loc.C > e.C {
		return false
	}
	return true
}
func (b *Buffer) clearSelection() {
	b.isSelecting = false
	b.selection.Start = b.cursor
	b.selection.End = b.cursor
	b.extraSelAnchors = nil
}
func (b *Buffer) selectAll() {
	b.isSelecting = true
	b.selection.Start = Loc{L: 0, C: 0, TargetX: -1}
	b.selection.End = Loc{L: len(b.lines) - 1, C: len(b.lines[len(b.lines)-1]), TargetX: -1}
	b.cursor = b.selection.End
}

// sliceText returns the buffer content spanning [s, e). Shared by
// getSelectedText and getMultiSelectedText so both build an identical
// multi-line join.
func (b *Buffer) sliceText(s, e Loc) string {
	s = b.clampLoc(s) // 🟢 [추가] 복사 시 좌표 이중 보정
	e = b.clampLoc(e) // 🟢 [추가] 복사 시 좌표 이중 보정
	if s.L == e.L {
		return string(b.lines[s.L][s.C:e.C])
	}
	var sb strings.Builder
	sb.Write(b.lines[s.L][s.C:])
	sb.WriteByte('\n')
	for i := s.L + 1; i < e.L; i++ {
		sb.Write(b.lines[i])
		sb.WriteByte('\n')
	}
	sb.Write(b.lines[e.L][:e.C])
	return sb.String()
}
func (b *Buffer) getSelectedText() string {
	if !b.HasSelection() {
		return ""
	}
	s, e := b.getSelectionRange()
	return b.sliceText(s, e)
}

// getMultiSelectedText gathers the text of every active selection -- the
// primary's and each extra cursor's own (extraSelAnchors) -- in ascending
// document order, joined by "\n". This is CodeMirror 6's copy/cut
// convention: N selections become N newline-joined pieces, which paste's
// byLine distribution (cursorRankMap) can later split back across N cursors.
// A cursor with no selection of its own contributes nothing (matching CM6:
// only non-empty ranges are collected). Degenerates to exactly
// getSelectedText()'s result when there is at most one selection, so
// existing single-selection copy/cut is unchanged.
func (b *Buffer) getMultiSelectedText() string {
	type piece struct {
		loc  Loc
		text string
	}
	var pieces []piece
	if b.HasSelection() {
		s, e := b.getSelectionRange()
		pieces = append(pieces, piece{loc: s, text: b.sliceText(s, e)})
	}
	if len(b.extraSelAnchors) == len(b.extraCursors) {
		for i, ec := range b.extraCursors {
			anchor := b.extraSelAnchors[i]
			// sameCaret, not ==: a TargetX-only difference would emit an empty
			// piece here, and the join would put a stray "\n" on the clipboard --
			// which also breaks the byLine paste round-trip, since the parts count
			// would no longer match the caret count.
			if sameCaret(anchor, ec) {
				continue
			}
			s, e := normalizeRange(anchor, ec)
			pieces = append(pieces, piece{loc: s, text: b.sliceText(s, e)})
		}
	}
	if len(pieces) == 0 {
		return ""
	}
	if len(pieces) == 1 {
		return pieces[0].text
	}
	// ascending sort by start position (insertion sort: selection count is tiny)
	for i := 1; i < len(pieces); i++ {
		x := pieces[i]
		j := i - 1
		for j >= 0 && (pieces[j].loc.L > x.loc.L || (pieces[j].loc.L == x.loc.L && pieces[j].loc.C > x.loc.C)) {
			pieces[j+1] = pieces[j]
			j--
		}
		pieces[j+1] = x
	}
	parts := make([]string, len(pieces))
	for i, p := range pieces {
		parts[i] = p.text
	}
	return strings.Join(parts, "\n")
}

// cursorRankMap gives each live caret's 0-based rank in ascending document
// order (primary + extras). Paste uses this to pair each caret with "its"
// line when the clipboard was copied from the same number of selections --
// CodeMirror 6's "byLine" paste distribution.
func (b *Buffer) cursorRankMap() map[Loc]int {
	all, _ := b.allCursorsSortedDesc()
	n := len(all)
	rank := make(map[Loc]int, n)
	for i, loc := range all {
		rank[loc] = n - 1 - i
	}
	return rank
}
func isWordChar(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
func (b *Buffer) selectWordAtCursor() {
	line := b.lines[b.cursor.L]
	if len(line) == 0 {
		return
	}
	c := b.cursor.C
	if c >= len(line) {
		c = len(line)
		_, size := utf8.DecodeLastRune(line)
		c -= size
	}

	r, _ := utf8.DecodeRune(line[c:])
	isSp := unicode.IsSpace(r)
	isAlpha := isWordChar(r)

	left := c
	for left > 0 {
		pr, psize := utf8.DecodeLastRune(line[:left])
		if isSp {
			if !unicode.IsSpace(pr) {
				break
			}
		} else {
			if unicode.IsSpace(pr) || isWordChar(pr) != isAlpha {
				break
			}
		}
		left -= psize
	}

	right := c
	for right < len(line) {
		cr, csize := utf8.DecodeRune(line[right:])
		if isSp {
			if !unicode.IsSpace(cr) {
				break
			}
		} else {
			if unicode.IsSpace(cr) || isWordChar(cr) != isAlpha {
				break
			}
		}
		right += csize
	}
	b.isSelecting = true
	b.selection.Start = Loc{L: b.cursor.L, C: left, TargetX: -1}
	b.selection.End = Loc{L: b.cursor.L, C: right, TargetX: -1}
	b.cursor = b.selection.End
}
func (b *Buffer) selectLineAtCursor() {
	b.isSelecting = true
	b.selection.Start = Loc{L: b.cursor.L, C: 0, TargetX: -1}
	b.selection.End = Loc{L: b.cursor.L, C: len(b.lines[b.cursor.L]), TargetX: -1}
	b.cursor = b.selection.End
}

// ponytail: multi-cursor helpers

// cleanExtraCursors removes duplicates and entries equal to the primary cursor.
// Keeps extras sorted ascending by (L, C).
// cursorDistance is a coarse line-dominant distance used only to pick the
// "nearest" cursor -- exact metric doesn't matter, just that same-line beats
// any other line.
func cursorDistance(a, b Loc) int {
	dl := a.L - b.L
	if dl < 0 {
		dl = -dl
	}
	dc := a.C - b.C
	if dc < 0 {
		dc = -dc
	}
	return dl*1_000_000 + dc
}

// removeCursorAt handles Ctrl+Click landing on an existing caret: clicking an
// extra cursor removes it (like micro's Ctrl+MouseLeft toggling a cursor
// off). Clicking the primary cursor can't simply delete it -- there must
// always be exactly one primary -- so instead the primary jumps to the
// nearest remaining cursor, and that cursor is absorbed.
// Returns false if loc doesn't match any existing caret, so the caller can
// fall back to adding a brand new cursor there.
func (b *Buffer) removeCursorAt(loc Loc) bool {
	// extraSelAnchors is index-aligned with extraCursors, so a splice on one is a
	// splice on the other. Dropping only the cursor makes every length guard fail,
	// which silently discards ALL remaining extra selections from rendering, copy,
	// and delete -- a routine Ctrl+Click would quietly destroy selection state.
	aligned := len(b.extraSelAnchors) == len(b.extraCursors)
	if !aligned {
		b.extraSelAnchors = nil // already desynced; see cleanExtraCursors
	}
	dropAt := func(i int) {
		b.extraCursors = append(b.extraCursors[:i], b.extraCursors[i+1:]...)
		if aligned {
			b.extraSelAnchors = append(b.extraSelAnchors[:i], b.extraSelAnchors[i+1:]...)
		}
	}
	for i, ec := range b.extraCursors {
		if sameCaret(ec, loc) {
			dropAt(i)
			return true
		}
	}
	if sameCaret(b.cursor, loc) && len(b.extraCursors) > 0 {
		nearest := 0
		best := -1
		for i, ec := range b.extraCursors {
			if d := cursorDistance(b.cursor, ec); best == -1 || d < best {
				best, nearest = d, i
			}
		}
		b.cursor = b.extraCursors[nearest]
		dropAt(nearest)
		return true
	}
	return false
}

// sameCaret reports whether two Locs denote the same caret position. Loc also
// carries TargetX (the memorized goal column for vertical movement), which is
// bookkeeping rather than position: a caret that leaves a line and comes back
// has the same L/C but a different TargetX. Every "is this the same spot?"
// test must therefore compare L/C only -- a bare `a == b` on two Locs silently
// treats a goal-column change as a moved caret.
func sameCaret(a, b Loc) bool {
	return a.L == b.L && a.C == b.C
}

// sameCaretSet compares two caret slices position-wise. Used by the render loop
// to decide whether the caret set changed since the last frame; TargetX is
// excluded for the reason given on sameCaret -- it never changes what is drawn.
func sameCaretSet(a, b []Loc) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameCaret(a[i], b[i]) {
			return false
		}
	}
	return true
}

// clearExtraCursors collapses multi-cursor mode. extraSelAnchors is
// index-aligned with extraCursors, so it must die with it -- otherwise a later
// cursor set of coincidentally equal length passes the alignment guard and
// adopts anchors from an unrelated edit session as real selections.
func (b *Buffer) clearExtraCursors() {
	b.extraCursors = nil
	b.extraSelAnchors = nil
}

func (b *Buffer) cleanExtraCursors() {
	if len(b.extraCursors) == 0 {
		b.extraSelAnchors = nil
		return
	}
	// extraSelAnchors is index-aligned with extraCursors, so every reorder and
	// every drop below has to be mirrored onto it. Otherwise the length guard at
	// the read sites stops matching and every extra selection silently vanishes
	// from rendering, copy, and delete.
	hasAnchors := len(b.extraSelAnchors) == len(b.extraCursors)
	if !hasAnchors {
		// Already unreadable (a caret was added or removed without its anchor).
		// Drop them NOW rather than leave them for a later caret set that happens
		// to have the same length to adopt as real selections -- those would
		// render highlighted and be deleted by the next Backspace.
		b.extraSelAnchors = nil
	}

	// sort ascending. Insertion sort rather than sort.Slice: cursor counts are
	// tiny and this runs on every multi-cursor edit/move, while sort.Slice's
	// reflection path allocates a boxed slice + swapper every call.
	ec := b.extraCursors
	an := b.extraSelAnchors
	for i := 1; i < len(ec); i++ {
		x := ec[i]
		var xa Loc
		if hasAnchors {
			xa = an[i]
		}
		j := i - 1
		for j >= 0 && (ec[j].L > x.L || (ec[j].L == x.L && ec[j].C > x.C)) {
			ec[j+1] = ec[j]
			if hasAnchors {
				an[j+1] = an[j]
			}
			j--
		}
		ec[j+1] = x
		if hasAnchors {
			an[j+1] = xa
		}
	}

	// deduplicate and remove any matching primary cursor
	out := make([]Loc, 0, len(b.extraCursors))
	var outAnchors []Loc
	if hasAnchors {
		outAnchors = make([]Loc, 0, len(b.extraSelAnchors))
	}
	var prev Loc
	hasPrev := false
	for i, ec := range b.extraCursors {
		ec = b.clampLoc(ec)
		if sameCaret(ec, b.cursor) {
			continue
		}
		if hasPrev && sameCaret(ec, prev) {
			continue
		}
		out = append(out, ec)
		if hasAnchors {
			outAnchors = append(outAnchors, b.clampLoc(b.extraSelAnchors[i]))
		}
		prev = ec
		hasPrev = true
	}
	b.extraCursors = out
	if hasAnchors {
		b.extraSelAnchors = outAnchors
	}
}

// allCursorsSortedDesc returns all cursors sorted descending, and the index of the primary cursor.
// Editing bottom-to-top keeps upper-cursor byte offsets valid.
func (b *Buffer) allCursorsSortedDesc() ([]Loc, int) {
	type cursorWithIdx struct {
		loc       Loc
		isPrimary bool
	}
	all := make([]cursorWithIdx, 0, 1+len(b.extraCursors))
	all = append(all, cursorWithIdx{b.cursor, true})
	for _, ec := range b.extraCursors {
		all = append(all, cursorWithIdx{ec, false})
	}
	// sort descending by (L, C). Insertion sort avoids sort.Slice's per-call
	// reflection allocation; the cursor set is small.
	for i := 1; i < len(all); i++ {
		x := all[i]
		j := i - 1
		for j >= 0 && (all[j].loc.L < x.loc.L || (all[j].loc.L == x.loc.L && all[j].loc.C < x.loc.C)) {
			all[j+1] = all[j]
			j--
		}
		all[j+1] = x
	}

	locs := make([]Loc, len(all))
	primaryIdx := 0
	for i, c := range all {
		locs[i] = c.loc
		if c.isPrimary {
			primaryIdx = i
		}
	}
	return locs, primaryIdx
}

// setCursorsFromDesc writes back an edited descending cursor list, preserving the primary cursor.
// cleanExtraCursors is called at the end to remove any extra cursor that has
// collapsed onto another cursor (e.g. when a backspace at column 0 merges two
// lines and two carets end up at the same position).
func (b *Buffer) setCursorsFromDesc(all []Loc, primaryIdx int) {
	if len(all) == 0 {
		return
	}
	if primaryIdx >= 0 && primaryIdx < len(all) {
		b.cursor = all[primaryIdx]
	} else {
		b.cursor = all[len(all)-1]
	}
	b.extraCursors = make([]Loc, 0, len(all)-1)
	// extras: all except primary, stored ascending
	for i := len(all) - 1; i >= 0; i-- {
		if i == primaryIdx {
			continue
		}
		b.extraCursors = append(b.extraCursors, all[i])
	}
	// Drop any extra cursor that has collapsed onto the primary or onto
	// another extra cursor (happens when a delete merges two lines and two
	// carets land at the same byte position).
	b.cleanExtraCursors()
}

// shiftLocForInsert maps a caret position q forward across an insertion that
// filled the span from loc to end. Positions before loc are untouched; the
// insertion point and everything after it slides by the inserted text's size.
//
// This is what a bottom-to-top multi-cursor edit must apply to the carets it has
// ALREADY processed: those carets sit at higher offsets than the current edit,
// and an insertion at a lower offset on the SAME line shifts their byte offsets.
// Without this, a recorded offset goes stale by however many bytes were inserted
// below it, and the next keystroke inserts mid-rune — splitting a multi-byte
// character into �� garbage.
func shiftLocForInsert(q, loc, end Loc) Loc {
	if q.L < loc.L || (q.L == loc.L && q.C < loc.C) {
		return q // strictly before the insertion point
	}
	lineDelta := end.L - loc.L
	if q.L == loc.L { // q.C >= loc.C: on the insertion line, at/after the point
		if lineDelta == 0 {
			q.C += end.C - loc.C
		} else {
			q.C = end.C + (q.C - loc.C)
			q.L += lineDelta
		}
	} else { // q on a later line: only its line index shifts
		q.L += lineDelta
	}
	q.TargetX = -1
	return q
}

// shiftLocForDelete maps a caret position q backward across a deletion of the
// span [start, end). It is the delete-side counterpart to shiftLocForInsert and
// serves the same purpose for multi-cursor backspace/delete.
func shiftLocForDelete(q, start, end Loc) Loc {
	if q.L < start.L || (q.L == start.L && q.C <= start.C) {
		return q // at or before the removed span
	}
	if q.L < end.L || (q.L == end.L && q.C <= end.C) {
		return Loc{L: start.L, C: start.C, TargetX: -1} // inside the removed span
	}
	if q.L == end.L { // on end's line, past it: merges onto start's line
		q.C = start.C + (q.C - end.C)
		q.L = start.L
	} else { // on a later line
		q.L -= end.L - start.L
	}
	q.TargetX = -1
	return q
}

// rebaseCaretsForEdit maps every extra caret and selection anchor across one
// delete-then-insert edit. The replace paths rewrite the buffer behind the caret
// set's back -- unlike runMultiCursorInsert/Delete, which drive the edit FROM the
// caret set -- so without this an extra caret keeps a byte offset that now points
// into the middle of a rune, or onto a line a "\n" replacement has since split.
// Caller is responsible for b.cursor and for a cleanExtraCursors() afterwards.
func (b *Buffer) rebaseCaretsForEdit(delStart, delEnd, insStart, insEnd Loc) {
	for i := range b.extraCursors {
		q := shiftLocForDelete(b.extraCursors[i], delStart, delEnd)
		b.extraCursors[i] = shiftLocForInsert(q, insStart, insEnd)
	}
	for i := range b.extraSelAnchors {
		q := shiftLocForDelete(b.extraSelAnchors[i], delStart, delEnd)
		b.extraSelAnchors[i] = shiftLocForInsert(q, insStart, insEnd)
	}
}

// normalizeRange orders s/e so s is the earlier position, matching
// getSelectionRange's swap-if-backwards convention. Used wherever a caret's
// own (anchor, live position) pair needs to become a renderable/deletable
// [start, end) range, since the live position can be on either side of the
// anchor depending on which direction the selection was extended.
func normalizeRange(s, e Loc) (Loc, Loc) {
	if s.L > e.L || (s.L == e.L && s.C > e.C) {
		return e, s
	}
	return s, e
}

// cellInSelRange reports whether the character at (lineIdx, col) -- col being
// a byte offset into that physical line -- falls inside a selection spanning
// [s, e). Shared by the primary selection's render highlighting and the
// per-extra-cursor selection highlighting so both use identical cell-boundary
// rules.
func cellInSelRange(lineIdx, col int, s, e Loc) bool {
	if lineIdx > s.L && lineIdx < e.L {
		return true
	}
	if lineIdx == s.L && lineIdx == e.L {
		return col >= s.C && col < e.C
	}
	if lineIdx == s.L && lineIdx < e.L {
		return col >= s.C
	}
	if lineIdx == e.L && lineIdx > s.L {
		return col < e.C
	}
	return false
}

// adjustCursorsAfterInsert rewrites every caret in all for an insertion that ran
// at index self (filling loc..end). The editing caret lands on end; every other
// caret is transformed so same-line carets above the edit stay on rune
// boundaries.
func adjustCursorsAfterInsert(all []Loc, self int, loc, end Loc) {
	for j := range all {
		if j == self {
			all[j] = end
		} else {
			all[j] = shiftLocForInsert(all[j], loc, end)
		}
	}
}

// adjustCursorsAfterDelete is the delete-side counterpart to
// adjustCursorsAfterInsert; the editing caret lands on start.
func adjustCursorsAfterDelete(all []Loc, self int, start, end Loc) {
	for j := range all {
		if j == self {
			all[j] = start
		} else {
			all[j] = shiftLocForDelete(all[j], start, end)
		}
	}
}

// runMultiCursorInsert and runMultiCursorDelete are the ONE shared bottom-to-top
// dance every multi-cursor-aware Insert/Delete key handler must go through.
// They exist so that adding a new multi-cursor feature (Ctrl+Backspace,
// multi-cursor Tab, paste, ...) only requires supplying what to insert/delete
// at each caret -- the caller can no longer forget the position-rebase step
// that caused the mojibake bug (a caret already edited this keystroke going
// stale when a later same-line edit landed at a lower offset, eventually
// splitting a multi-byte rune).
//
// This does NOT touch Insert/Remove or Undo/Redo: those are replayed directly
// by Undo()/Redo() using each transaction's own recorded Actions, and already
// have their own correct-by-construction cursor restoration
// (restoreExtraCursorsFromActions) that deliberately does NOT resurrect
// dismissed cursors. Rebasing at that lower level would fire on every replayed
// action and fight with that restoration.
func (b *Buffer) runMultiCursorInsert(textFor func(loc Loc, isPrimary bool) string) {
	// Fast path: with no extra cursors this is just a single-cursor insert.
	// InsertTextWithRecord already jumps b.cursor to the insertion end, which is
	// exactly what the sort/adjust/write-back dance below produces for one
	// caret -- so skip that machinery (allCursorsSortedDesc's sort + slice
	// allocations run on every keystroke otherwise, the common case).
	if len(b.extraCursors) == 0 {
		b.nextActionIsExtra = false
		b.InsertTextWithRecord(b.cursor, textFor(b.cursor, true))
		return
	}
	all, primaryIdx := b.allCursorsSortedDesc()
	for i, c := range all {
		b.cursor = c
		b.nextActionIsExtra = i != primaryIdx
		b.InsertTextWithRecord(b.cursor, textFor(c, i == primaryIdx))
		adjustCursorsAfterInsert(all, i, c, b.cursor)
	}
	b.nextActionIsExtra = false
	b.setCursorsFromDesc(all, primaryIdx)
}

func (b *Buffer) runMultiCursorDelete(rangeFor func(loc Loc, isPrimary bool) (start, end Loc)) {
	// Fast path: see runMultiCursorInsert. DeleteTextWithRecord jumps b.cursor to
	// the deletion start, matching the single-caret result of the loop below.
	if len(b.extraCursors) == 0 {
		b.nextActionIsExtra = false
		start, end := rangeFor(b.cursor, true)
		b.DeleteTextWithRecord(start, end)
		return
	}
	all, primaryIdx := b.allCursorsSortedDesc()
	for i, c := range all {
		b.cursor = c
		b.nextActionIsExtra = i != primaryIdx
		start, end := rangeFor(c, i == primaryIdx)
		b.DeleteTextWithRecord(start, end)
		adjustCursorsAfterDelete(all, i, start, end)
	}
	b.nextActionIsExtra = false
	b.setCursorsFromDesc(all, primaryIdx)
}

// runMultiCursorDedent removes up to tabSize of leading whitespace (or a
// single leading tab) once per DISTINCT line touched by a live caret. Unlike
// insert/delete, dedent is inherently a per-LINE operation, not a per-caret
// one: two carets sharing a line must not have that line's indentation
// stripped twice, so this can't reuse runMultiCursorDelete's one-edit-per-
// caret loop.
//
// Because one dedent action may need to represent more than one extra caret
// (when several share a line), it doesn't fit the transaction model's
// one-action-per-caret assumption that undo uses to restore extra-cursor
// positions (restoreExtraCursorsFromActions). Content undo is always exact;
// if extra carets shared a dedented line, their columns after undoing a
// dedent may not perfectly revert -- a narrow, accepted trade-off rather than
// a content-correctness bug.
func (b *Buffer) runMultiCursorDedent(tabSize int) {
	primary := b.cursor
	extras := append([]Loc(nil), b.extraCursors...)

	lineSet := map[int]bool{primary.L: true}
	for _, ec := range extras {
		lineSet[ec.L] = true
	}
	lines := make([]int, 0, len(lineSet))
	for l := range lineSet {
		lines = append(lines, l)
	}
	sort.Ints(lines)

	for _, r := range lines {
		line := b.lines[r]
		if len(line) == 0 {
			continue
		}
		removeCount := 0
		if line[0] == '\t' {
			removeCount = 1
		} else {
			for removeCount < len(line) && removeCount < tabSize && line[removeCount] == ' ' {
				removeCount++
			}
		}
		if removeCount == 0 {
			continue
		}
		// DeleteTextWithRecord snapshots `anchor := b.cursor` to remember which
		// caret produced the action, so b.cursor must BE that caret. Without this
		// each action inherits the previous iteration's landing spot, and undo
		// then restores the extra carets onto the wrong lines entirely.
		owner := primary
		if primary.L != r {
			for _, ec := range extras {
				if ec.L == r {
					owner = ec
					break
				}
			}
		}
		b.cursor = owner
		b.nextActionIsExtra = primary.L != r
		b.DeleteTextWithRecord(Loc{L: r, C: 0, TargetX: -1}, Loc{L: r, C: removeCount, TargetX: -1})

		if primary.L == r {
			primary.C -= removeCount
			if primary.C < 0 {
				primary.C = 0
			}
		}
		for i := range extras {
			if extras[i].L == r {
				extras[i].C -= removeCount
				if extras[i].C < 0 {
					extras[i].C = 0
				}
			}
		}
	}
	b.nextActionIsExtra = false
	b.cursor = primary
	b.extraCursors = extras
	// 💡 필수: 같은 줄의 두 커서가 들여쓰기 제거 후 모두 C=0으로 클램프되면 겹친다.
	// 정리하지 않으면 다음 입력이 그 자리에 두 번 삽입된다.
	b.cleanExtraCursors()
}

// runMultiCursorMove runs move once for the primary cursor, then once more for
// each extra cursor (temporarily aliasing b.cursor to it so the closure's own
// b.cursor-reading logic -- word/paragraph movement, soft-wrap sub-line
// lookup, TargetX threading -- applies identically to every caret).
//
// Every movement key (Left/Right/Up/Down/PgUp/PgDn/Home/End) used to hand-roll
// this as "move the primary, then copy-paste the same logic into a loop over
// extraCursors" -- the same duplication pattern the edit handlers had before
// they were unified onto runMultiCursorInsert/Delete. This is the movement-side
// equivalent: with zero extra cursors it degenerates to a single move() call,
// so there's no separate single-cursor code path.
func (b *Buffer) runMultiCursorMove(move func()) {
	move()
	for i := range b.extraCursors {
		saved := b.cursor
		b.cursor = b.extraCursors[i]
		move()
		b.extraCursors[i] = b.cursor
		b.cursor = saved
	}
	if len(b.extraCursors) > 0 {
		b.cleanExtraCursors()
	}
}

// addCursorAbove adds a new caret one visual line above the topmost existing
// caret (primary or extra), so repeated presses keep walking upward instead
// of always measuring from the (unmoved) primary cursor.
func (b *Buffer) addCursorAbove(cfg Config) {
	origCursor := b.cursor
	origExtras := append([]Loc(nil), b.extraCursors...)
	all, _ := b.allCursorsSortedDesc() // descending: last entry is topmost (smallest L)
	top := all[len(all)-1]

	b.cursor = top
	origSub := b.getCursorSub(cfg)
	tx := top.TargetX
	b.moveCursorVisualLine(-1, cfg, &tx)
	newCaret := b.cursor
	newCaret.TargetX = tx
	// At the top of the buffer moveCursorVisualLine clamps instead of moving,
	// snapping the caret to the top of its own visual line. Adding a caret there
	// would put two carets on ONE visual line, so every keystroke types twice.
	moved := newCaret.L != top.L || b.getCursorSub(cfg) != origSub

	b.cursor = origCursor
	if !moved {
		b.extraCursors = origExtras
		b.cleanExtraCursors()
		return
	}
	b.extraCursors = append(origExtras, newCaret)
	b.cleanExtraCursors()
}

// addCursorBelow mirrors addCursorAbove, extending from the bottommost caret.
func (b *Buffer) addCursorBelow(cfg Config) {
	origCursor := b.cursor
	origExtras := append([]Loc(nil), b.extraCursors...)
	all, _ := b.allCursorsSortedDesc() // descending: first entry is bottommost (largest L)
	bottom := all[0]

	b.cursor = bottom
	origSub := b.getCursorSub(cfg)
	tx := bottom.TargetX
	b.moveCursorVisualLine(1, cfg, &tx)
	newCaret := b.cursor
	newCaret.TargetX = tx
	// At the bottom of the buffer moveCursorVisualLine clamps instead of moving,
	// snapping the caret to the bottom of its own visual line. Adding a caret there
	// would put two carets on ONE visual line, so every keystroke types twice.
	moved := newCaret.L != bottom.L || b.getCursorSub(cfg) != origSub

	b.cursor = origCursor
	if !moved {
		b.extraCursors = origExtras
		b.cleanExtraCursors()
		return
	}
	b.extraCursors = append(origExtras, newCaret)
	b.cleanExtraCursors()
}

// 💡 현재 줄과 윗줄을 통째로 맞바꾼다. 두 줄을 한 번에 지우고 순서를 뒤집어
// 다시 넣기 때문에 undo 한 스텝으로 되돌아간다.
func (b *Buffer) moveLineUp() {
	if b.cursor.L <= 0 {
		return
	}
	oldL, oldC := b.cursor.L, b.cursor.C // 💡 안전하게 원본 위치 캡처
	b.BeginTransaction()
	currStr := string(b.lines[oldL])
	prevStr := string(b.lines[oldL-1])
	b.DeleteTextWithRecord(Loc{L: oldL - 1, C: 0, TargetX: -1}, Loc{L: oldL, C: len(b.lines[oldL]), TargetX: -1})
	b.InsertTextWithRecord(Loc{L: oldL - 1, C: 0, TargetX: -1}, currStr+"\n"+prevStr)
	b.cursor = b.clampLoc(Loc{L: oldL - 1, C: oldC, TargetX: -1}) // 💡 절대 에러 방지
	b.EndTransaction()
}

func (b *Buffer) moveLineDown() {
	if b.cursor.L >= len(b.lines)-1 {
		return
	}
	oldL, oldC := b.cursor.L, b.cursor.C // 💡 안전하게 원본 위치 캡처
	b.BeginTransaction()
	currStr := string(b.lines[oldL])
	nextStr := string(b.lines[oldL+1])
	b.DeleteTextWithRecord(Loc{L: oldL, C: 0, TargetX: -1}, Loc{L: oldL + 1, C: len(b.lines[oldL+1]), TargetX: -1})
	b.InsertTextWithRecord(Loc{L: oldL, C: 0, TargetX: -1}, nextStr+"\n"+currStr)
	b.cursor = b.clampLoc(Loc{L: oldL + 1, C: oldC, TargetX: -1}) // 💡 절대 에러 방지
	b.EndTransaction()
}

func (b *Buffer) moveWordLeft() {
	if b.cursor.C == 0 {
		if b.cursor.L > 0 {
			b.cursor.L--
			b.cursor.C = len(b.lines[b.cursor.L])
		}
		return
	}
	line := b.lines[b.cursor.L]
	for b.cursor.C > 0 {
		r, size := utf8.DecodeLastRune(line[:b.cursor.C])
		if !unicode.IsSpace(r) {
			break
		}
		b.cursor.C -= size
	}
	if b.cursor.C == 0 {
		return
	}
	r, _ := utf8.DecodeLastRune(line[:b.cursor.C])
	isAlpha := isWordChar(r)
	for b.cursor.C > 0 {
		pr, psize := utf8.DecodeLastRune(line[:b.cursor.C])
		if unicode.IsSpace(pr) || isWordChar(pr) != isAlpha {
			break
		}
		b.cursor.C -= psize
	}
}
func (b *Buffer) moveWordRight() {
	line := b.lines[b.cursor.L]
	if b.cursor.C == len(line) {
		if b.cursor.L < len(b.lines)-1 {
			b.cursor.L++
			b.cursor.C = 0
		}
		return
	}
	r, _ := utf8.DecodeRune(line[b.cursor.C:])
	isAlpha := isWordChar(r)
	for b.cursor.C < len(line) {
		cr, csize := utf8.DecodeRune(line[b.cursor.C:])
		if unicode.IsSpace(cr) || isWordChar(cr) != isAlpha {
			break
		}
		b.cursor.C += csize
	}
	for b.cursor.C < len(line) {
		cr, csize := utf8.DecodeRune(line[b.cursor.C:])
		if !unicode.IsSpace(cr) {
			break
		}
		b.cursor.C += csize
	}
}
func (b *Buffer) moveParagraphUp() {
	curr := b.cursor.L - 1
	for curr >= 0 && len(b.lines[curr]) == 0 {
		curr--
	}
	for curr >= 0 && len(b.lines[curr]) > 0 {
		curr--
	}
	if curr < 0 {
		b.cursor.L = 0
	} else {
		b.cursor.L = curr
	}
	b.cursor.C = 0
}
func (b *Buffer) moveParagraphDown() {
	curr := b.cursor.L + 1
	for curr < len(b.lines) && len(b.lines[curr]) == 0 {
		curr++
	}
	for curr < len(b.lines) && len(b.lines[curr]) > 0 {
		curr++
	}
	if curr >= len(b.lines) {
		b.cursor.L = len(b.lines) - 1
	} else {
		b.cursor.L = curr
	}
	b.cursor.C = 0
}
func (b *Buffer) isLineUnwrapped(lineIdx int, cfg Config) bool {
	if lineIdx < 0 || lineIdx >= len(b.lines) {
		return false
	}
	// 💡 래핑 옵션이 꺼져 있을 때만 true를 반환하는 원래의 완벽한 상태로 복구!
	return !cfg.LineWrapping
}

func (b *Buffer) getLineNumWidth(cfg Config) int {
	if !cfg.ShowLineNumbers {
		return 0
	}
	totalLines := len(b.lines)
	digits := 0
	for totalLines > 0 {
		digits++
		totalLines /= 10
	}
	if digits < 4 {
		digits = 4
	}
	return digits + 1
}

// 💡 4. 부분 계산(Per-Line Cache) 엔진 - 100만 줄도 0.001초 컷
// 🟢 [변경] 특정 물리 줄의 visual line 캐시가 비어 있으면 동적으로 계산해주는 헬퍼
func (b *Buffer) ensureVCache(i int, cfg Config) []VisualLine {
	if b.vCache == nil {
		b.vCache = make(map[int][]VisualLine)
	}
	if val, ok := b.vCache[i]; ok {
		return val
	}
	line := b.lines[i]
	lineNumWidth := b.getLineNumWidth(cfg)
	textMaxWidth := b.cachedMaxWidth - lineNumWidth
	if textMaxWidth <= 0 {
		textMaxWidth = 1
	}

	var temp []VisualLine
	if !cfg.LineWrapping || len(line) == 0 {
		width := 0
		for i := 0; i < len(line); {
			r, size := utf8.DecodeRune(line[i:])
			width += fastRuneWidth(r, cfg.TabSize)
			i += size
		}
		temp = []VisualLine{{isWrapped: false, startCX: 0, endCX: len(line), width: width}}
	} else {
		start, currentX := 0, 0
		cx := 0
		for cx < len(line) {
			r, size := utf8.DecodeRune(line[cx:])
			rw := fastRuneWidth(r, cfg.TabSize)
			if currentX+rw > textMaxWidth {
				temp = append(temp, VisualLine{isWrapped: start > 0, startCX: start, endCX: cx, width: currentX})
				start = cx
				currentX = 0
			}
			currentX += rw
			cx += size
		}
		temp = append(temp, VisualLine{isWrapped: start > 0, startCX: start, endCX: len(line), width: currentX})
	}
	b.vCache[i] = temp
	return temp
}

// 🟢 [변경] O(1) 크기 무효화만 수행
func (b *Buffer) generateVisualLines(maxWidth int, cfg Config) []VisualLine {
	if b.vCache == nil {
		b.vCache = make(map[int][]VisualLine)
	}
	if b.cachedMaxWidth != maxWidth || b.cachedConfig.LineWrapping != cfg.LineWrapping || b.cachedConfig.TabSize != cfg.TabSize || b.cachedConfig.ShowLineNumbers != cfg.ShowLineNumbers || len(b.vCache) > 1000 {
		b.vCache = make(map[int][]VisualLine) // 💡 맵 초기화
		b.cachedMaxWidth = maxWidth
		b.cachedConfig = cfg

		if b.vOffsetL >= len(b.lines) {
			b.vOffsetL = len(b.lines) - 1
		}
		if b.vOffsetL < 0 {
			b.vOffsetL = 0
		}
		b.ensureVCache(b.vOffsetL, cfg)
		if b.vOffsetSub >= len(b.vCache[b.vOffsetL]) {
			b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
		}
		if b.vOffsetSub < 0 {
			b.vOffsetSub = 0
		}
	}
	b.vLinesValid = true
	return nil
}

// 🟢 [추가] 커서가 물리 줄 내에서 가상 래핑 몇 번째 줄에 위치하는지 구하는 O(1) 함수
func (b *Buffer) getCursorSub(cfg Config) int {
	b.ensureVCache(b.cursor.L, cfg)
	vcls := b.vCache[b.cursor.L]
	for idx, vl := range vcls {
		if b.cursor.C >= vl.startCX && b.cursor.C <= vl.endCX {
			if b.cursor.C == vl.endCX && idx+1 < len(vcls) {
				if b.stickToWrapEnd {
					return idx
				}
				continue
			}
			return idx
		}
	}
	return 0
}

// 🟢 [변경] 화면 범위(Height) 내에서만 루프를 돌아 스크롤을 맞추는 O(Height) 스크롤 추적기
func (b *Buffer) scrollToCursorV(cfg Config, screenHeight int) {
	if screenHeight <= 0 || len(b.lines) == 0 {
		return
	}
	b.cursor = b.clampLoc(b.cursor)
	cursorSub := b.getCursorSub(cfg)

	// 화면 상단보다 위에 있는 경우
	if b.cursor.L < b.vOffsetL || (b.cursor.L == b.vOffsetL && cursorSub < b.vOffsetSub) {
		b.vOffsetL = b.cursor.L
		b.vOffsetSub = cursorSub
		return
	}

	// 화면 상단 아래에 있는 경우, 화면 높이만큼만 내려가면서 오프셋 검사
	visualLinesCount := 0
	currL := b.vOffsetL
	currSub := b.vOffsetSub

	for currL < b.cursor.L || (currL == b.cursor.L && currSub < cursorSub) {
		visualLinesCount++
		if visualLinesCount > screenHeight {
			break
		}
		b.ensureVCache(currL, cfg)
		currSub++
		if currSub >= len(b.vCache[currL]) {
			currSub = 0
			currL++
		}
	}

	// 화면 높이를 초과하여 아래에 있는 경우 역방향으로 화면 오프셋을 끌어올림
	if visualLinesCount >= screenHeight {
		b.vOffsetL = b.cursor.L
		b.vOffsetSub = cursorSub

		for i := 0; i < screenHeight-1; i++ {
			b.vOffsetSub--
			if b.vOffsetSub < 0 {
				if b.vOffsetL > 0 {
					b.vOffsetL--
					b.ensureVCache(b.vOffsetL, cfg)
					b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
				} else {
					b.vOffsetSub = 0
					break
				}
			}
		}
	}
}

func (b *Buffer) scrollToCursorH(textMaxWidth int, cfg Config) {
	if textMaxWidth <= 0 {
		return
	}
	b.cursor = b.clampLoc(b.cursor)

	cursorSub := b.getCursorSub(cfg)
	vl := b.vCache[b.cursor.L][cursorSub]

	line := b.lines[b.cursor.L]
	cursorVisX := 0

	for i := vl.startCX; i < b.cursor.C && i < vl.endCX && i < len(line); {
		r, size := utf8.DecodeRune(line[i:])
		rw := fastRuneWidth(r, cfg.TabSize)
		cursorVisX += rw
		i += size
	}

	charWidth := 1
	if b.cursor.C < len(line) && b.cursor.C >= vl.startCX && b.cursor.C < vl.endCX {
		r, _ := utf8.DecodeRune(line[b.cursor.C:])
		charWidth = runewidth.RuneWidth(r)
		if r == '\t' {
			charWidth = cfg.TabSize
		}
	}

	startVisX := cursorVisX
	endVisX := cursorVisX + charWidth
	if !b.isLineUnwrapped(b.cursor.L, cfg) {
		if endVisX > textMaxWidth {
			endVisX = textMaxWidth
		}
	}

	if endVisX > b.hOffset+textMaxWidth {
		b.hOffset = endVisX - textMaxWidth
	}
	if startVisX < b.hOffset {
		b.hOffset = startVisX
	}
}

// 🟢 [변경] 화면 Y 좌표만큼만 오프셋을 동적으로 내려가서 해당 텍스트의 물리/가상 인덱스를 역산하는 O(Screen Height) 메모리 변환기
func (b *Buffer) screenToMemoryPosV(mx, my, tabHeight int, cfg Config) Loc {
	lineNumWidth := b.getLineNumWidth(cfg)
	if len(b.lines) == 0 {
		return Loc{L: 0, C: 0, TargetX: -1}
	}

	relativeY := my - tabHeight
	if relativeY < 0 {
		relativeY = 0
	}

	currL := b.vOffsetL
	currSub := b.vOffsetSub

	for i := 0; i < relativeY; i++ {
		b.ensureVCache(currL, cfg)
		currSub++
		if currSub >= len(b.vCache[currL]) {
			currSub = 0
			currL++
		}
		if currL >= len(b.lines) {
			currL = len(b.lines) - 1
			b.ensureVCache(currL, cfg)
			currSub = len(b.vCache[currL]) - 1
			break
		}
	}

	if currL >= len(b.lines) {
		currL = len(b.lines) - 1
	}
	b.ensureVCache(currL, cfg)
	subLines := b.vCache[currL]
	if len(subLines) == 0 {
		return Loc{L: currL, C: 0, TargetX: -1}
	}
	if currSub >= len(subLines) {
		currSub = len(subLines) - 1
	}
	if currSub < 0 {
		currSub = 0
	}
	vl := subLines[currSub]

	line := b.lines[currL]

	safeStart := vl.startCX
	if safeStart < 0 {
		safeStart = 0
	}
	if safeStart > len(line) {
		safeStart = len(line)
	}

	safeEnd := vl.endCX
	if safeEnd < safeStart {
		safeEnd = safeStart
	}
	if safeEnd > len(line) {
		safeEnd = len(line)
	}

	relativeX := mx - lineNumWidth + b.hOffset
	if relativeX <= 0 {
		return Loc{L: currL, C: safeStart, TargetX: -1}
	}

	currentX := 0

	for i := safeStart; i < safeEnd; {
		if i >= len(line) {
			break
		}
		r, size := utf8.DecodeRune(line[i:])
		if size <= 0 || i+size > len(line) {
			break
		}
		rw := fastRuneWidth(r, cfg.TabSize)

		if relativeX >= currentX && relativeX < currentX+rw {
			if rw > 1 && relativeX >= currentX+(rw/2)+(rw%2) {
				nextPos := i + size
				if nextPos > len(line) {
					nextPos = len(line)
				}
				return Loc{L: currL, C: nextPos, TargetX: -1}
			}
			return Loc{L: currL, C: i, TargetX: -1}
		}
		currentX += rw
		i += size
	}

	return Loc{L: currL, C: safeEnd, TargetX: -1}
}

// 🟢 [추가] 위/아래(Up/Down/PgUp/PgDn) 키를 눌렀을 때, 누적합 없이 인접 가상라인으로 커서를 안전하게 옮기는 O(1) 헬퍼
func (b *Buffer) moveCursorVisualLine(delta int, cfg Config, targetX *int) {
	b.cursor = b.clampLoc(b.cursor)
	cursorSub := b.getCursorSub(cfg)
	b.ensureVCache(b.cursor.L, cfg)
	currentVL := b.vCache[b.cursor.L][cursorSub]

	// 💡 1. 현재 커서의 시각적 X 오프셋 계산 (바이트가 아닌 화면 칸 수 기준)
	visualOffset := 0
	if targetX != nil && *targetX != -1 {
		visualOffset = *targetX
	} else {
		currLineData := b.lines[b.cursor.L]
		for i := currentVL.startCX; i < b.cursor.C && i < currentVL.endCX && i < len(currLineData); {
			r, size := utf8.DecodeRune(currLineData[i:])
			visualOffset += fastRuneWidth(r, cfg.TabSize)
			i += size
		}
		if targetX != nil {
			*targetX = visualOffset
		}
	}

	currL := b.cursor.L
	currSub := cursorSub + delta

	if delta < 0 {
		for currSub < 0 {
			if currL > 0 {
				currL--
				b.ensureVCache(currL, cfg)
				currSub += len(b.vCache[currL])
			} else {
				currL = 0
				currSub = 0
				// 버퍼 맨 위에서는 이번 이동만 0열로 스냅한다. *targetX에 되쓰면
				// 호출자가 기억하던 목표 열이 영구히 지워져, 다시 Down으로 내려올 때
				// 원래 열로 돌아오지 못한다 — TargetX가 존재하는 이유 그 자체.
				visualOffset = 0
				break
			}
		}
	} else if delta > 0 {
		for {
			b.ensureVCache(currL, cfg)
			if currSub < len(b.vCache[currL]) {
				break
			}
			if currL+1 < len(b.lines) {
				currSub -= len(b.vCache[currL])
				currL++
			} else {
				currL = len(b.lines) - 1
				b.ensureVCache(currL, cfg)
				currSub = len(b.vCache[currL]) - 1
				// 위쪽 클램프와 대칭: 이번 이동만 줄 끝으로 스냅하고 목표 열은 보존한다.
				// (되쓰면 addCursorBelow가 새 커서 TargetX에 999999를 굽는다.)
				visualOffset = 999999 // 끝으로 강제 정렬
				break
			}
		}
	}

	b.cursor.L = currL
	b.ensureVCache(currL, cfg)
	targetVL := b.vCache[currL][currSub]

	// 💡 2. 타겟 라인에서 시각적 오프셋에 가장 일치하는 바이트 인덱스 찾기
	targetC := targetVL.startCX
	targetVisOffset := 0
	targetLine := b.lines[currL]

	for i := targetVL.startCX; i < targetVL.endCX && i < len(targetLine); {
		r, size := utf8.DecodeRune(targetLine[i:])
		rw := fastRuneWidth(r, cfg.TabSize)

		// 화면 좌표 절반을 넘어가면 해당 글자의 위치로 스냅
		if targetVisOffset+(rw/2) >= visualOffset {
			targetC = i
			break
		}

		targetVisOffset += rw
		targetC = i + size
		i += size
	}

	b.cursor.C = targetC
	if b.cursor.C > targetVL.endCX {
		b.cursor.C = targetVL.endCX
	}
}
func sprintfRight(num, width int) string {
	// 💡 문자열 반복 연산을 없애고 표준 라이브러리를 통해 즉시 공간 정렬
	return fmt.Sprintf("%*d", width, num)
}

// 💡 추가됨: 파일명이 겹치면 부모 폴더까지 보여주는 스마트 타이틀 함수
func (e *Editor) getTabTitle(bufIdx int) string {
	b := e.buffers[bufIdx]
	if b.filePath == "" {
		if b.isConfig {
			return "config.json"
		}
		return "Untitled"
	}

	fileName := filepath.Base(b.filePath)

	// 다른 탭 중에 같은 파일명을 가진 탭이 있는지 검사
	isDuplicate := false
	for i, other := range e.buffers {
		if i != bufIdx && other.filePath != "" && filepath.Base(other.filePath) == fileName {
			isDuplicate = true
			break
		}
	}

	// 이름이 겹치면 "부모폴더/파일명" 형태로 반환
	if isDuplicate {
		parent := filepath.Base(filepath.Dir(b.filePath))
		return filepath.Join(parent, fileName)
	}

	return fileName
}

func (e *Editor) draw(s tcell.Screen) {
	w, h := s.Size()
	if h <= 0 || w <= 0 {
		return
	}

	b := e.getActive()

	// 🟢 [완벽 최적화 엔진] 전체 렌더링 vs 부분 렌더링 분기점
	forceAllDirty := false
	isFastPath := false

	if e.needsFullRefresh ||
		e.activeBuffer != e.prevActiveBuf ||
		b.vOffsetL != e.prevVOffset || // 🟢 변경
		b.vOffsetSub != e.prevVOffsetSub || // 🟢 추가
		b.hOffset != e.prevHOffset ||
		b.selection.Start != e.prevSelStart ||
		b.selection.End != e.prevSelEnd ||
		b.isSelecting != e.prevIsSelecting ||
		e.paletteActive || e.prevPalette ||
		e.ctxMenuActive || e.prevCtxMenu ||
		e.encodeMenuActive || e.prevEncode ||
		e.keyMenuActive || e.prevKeyMenu ||
		e.promptMode || e.prevPrompt ||
		b.searchMode || e.prevSearch ||
		b.gotoMode || e.prevGoto || len(b.lines) != e.prevLinesLen ||
		!sameCaretSet(b.extraCursors, e.prevExtraCursors) ||
		!sameCaretSet(b.extraSelAnchors, e.prevExtraSelAnchors) {
		forceAllDirty = true
	} else {
		if b.totalChars == e.prevTotalChars && b.txIDCounter == e.prevTxID {
			isFastPath = true
		}
	}

	if e.needsFullRefresh {
		s.Clear()
		e.needsFullRefresh = false
	}
	setCell := func(x, y int, r rune, comb []rune, style tcell.Style) {
		if x >= 0 && x < w && y >= 0 && y < h {
			s.SetContent(x, y, r, comb, style)
		}
	}

	lineNumWidth := b.getLineNumWidth(e.cfg)
	textMaxWidth := w - lineNumWidth
	if textMaxWidth <= 0 {
		textMaxWidth = 1
	}

	tabStyle := tcell.StyleDefault.Background(tcell.ColorDarkGray).Foreground(tcell.ColorWhite)
	activeTabStyle := tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorWhite).Bold(true)

	e.tabBounds = []TabBound{}
	tabX, tabY := 0, 0

	for i, buf := range e.buffers {
		name := e.getTabTitle(i)
		if buf.isModified {
			name += " *"
		}
		if buf.isReadOnly {
			name += " 🔒"
		}
		tabStr := " [" + name + "] "
		tabLen := runewidth.StringWidth(tabStr)

		if tabX+tabLen > w {
			for x := tabX; x < w; x++ {
				setCell(x, tabY, ' ', nil, tabStyle)
			}
			tabX = 0
			tabY++
		}
		currentStyle := tabStyle
		if i == e.activeBuffer {
			currentStyle = activeTabStyle
		}
		startX := tabX
		for _, r := range tabStr {
			setCell(tabX, tabY, r, nil, currentStyle)
			tabX += runewidth.RuneWidth(r)
		}
		e.tabBounds = append(e.tabBounds, TabBound{Idx: i, StartX: startX, EndX: tabX, Y: tabY})
	}
	for x := tabX; x < w; x++ {
		setCell(x, tabY, ' ', nil, tabStyle)
	}
	e.tabHeight = tabY + 1

	cursorVX, cursorVY := -1, -1
	currentRenderY := e.tabHeight

	defaultStyle := tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorDefault)
	lineNumStyle := tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorDarkGray)
	statusStyle := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack)
	selectedStyle := tcell.StyleDefault.Background(tcell.ColorDeepSkyBlue).Foreground(tcell.ColorWhite)
	highlightStyle := tcell.StyleDefault.Background(tcell.Color236).Foreground(tcell.ColorDefault)
	matchHighlightStyle := tcell.StyleDefault.Background(tcell.ColorYellow).Foreground(tcell.ColorBlack)

	selStart, selEnd := b.getSelectionRange()
	selStart = b.clampLoc(selStart) // 🟢 선택 영역 좌표 보정
	selEnd = b.clampLoc(selEnd)     // 🟢 선택 영역 좌표 보정
	hasSel := b.HasSelection()
	// ponytail: each extra cursor can carry its own independent
	// selection (extraSelAnchors, index-aligned with extraCursors).
	// Only trusted when the counts match -- see the field's doc comment.
	hasExtraSel := len(b.extraSelAnchors) == len(b.extraCursors) && len(b.extraSelAnchors) > 0

	// ponytail: per-line scratch for the extra-cursor lookups hoisted out of the
	// character loop below. Reused across rows so a multi-cursor repaint doesn't
	// allocate once per visual line.
	var lineExtraCols []int
	var lineExtraSels [][2]Loc

	// 🟢 [변경] 누적합 대신, 오프셋 줄부터 한 줄씩 증가하며 화면 높이만큼만 렌더링
	currL := b.vOffsetL
	currSub := b.vOffsetSub

	for currentRenderY < h-1 {
		if currL >= len(b.lines) {
			break
		}
		b.ensureVCache(currL, e.cfg)
		vcls := b.vCache[currL]
		if currSub >= len(vcls) {
			currSub = len(vcls) - 1
			if currSub < 0 {
				currSub = 0
			}
		}

		vl := vcls[currSub]
		lineIdx := currL
		lineData := b.lines[lineIdx]

		isRowDirty := forceAllDirty
		if !isRowDirty {
			if isFastPath {
				if lineIdx == b.cursor.L || lineIdx == e.prevCursor.L {
					isRowDirty = true
				}
			} else {
				if (b.dirtyStartL != -1 && lineIdx >= b.dirtyStartL) || lineIdx == b.cursor.L || lineIdx == e.prevCursor.L {
					isRowDirty = true
				}
			}
		}

		if !isRowDirty {
			currentRenderY++
			currSub++
			if currSub >= len(vcls) {
				currSub = 0
				currL++
			}
			continue
		}

		for x := 0; x < w; x++ {
			setCell(x, currentRenderY, ' ', nil, tcell.StyleDefault)
		}

		var lineMatches []MatchInfo
		if b.searchMode && len(b.searchQuery) > 0 && len(b.matches) > 0 {
			startIdx := sort.Search(len(b.matches), func(idx int) bool {
				return b.matches[idx].loc.L >= lineIdx
			})
			for idx := startIdx; idx < len(b.matches); idx++ {
				m := b.matches[idx]
				if m.loc.L > lineIdx {
					break
				}
				lineMatches = append(lineMatches, m)
			}
		}

		lineStyle := defaultStyle
		if e.cfg.HighlightLine && lineIdx == b.cursor.L && !b.isSelecting {
			lineStyle = highlightStyle
		}
		for x := lineNumWidth; x < w; x++ {
			setCell(x, currentRenderY, ' ', nil, lineStyle)
		}

		if e.cfg.ShowLineNumbers {
			if !vl.isWrapped {
				lineNumStr := sprintfRight(lineIdx+1, lineNumWidth-1) + " "
				for x, r := range lineNumStr {
					setCell(x, currentRenderY, r, nil, lineNumStyle)
				}
			} else {
				for x := 0; x < lineNumWidth; x++ {
					setCell(x, currentRenderY, ' ', nil, lineNumStyle)
				}
			}
		}

		// ponytail: collect this line's extra carets and extra selections ONCE,
		// before the character loop. Rescanning b.extraCursors per rune made the
		// render hot path O(chars x extraCursors) and -- worse -- kept the
		// horizontal-clipping break below from ever firing on a line holding an
		// extra caret further right, so a long minified line was fully decoded and
		// styled every frame.
		lineExtraCols = lineExtraCols[:0]
		maxExtraC := -1
		for _, ec := range b.extraCursors {
			if ec.L == lineIdx {
				lineExtraCols = append(lineExtraCols, ec.C)
				if ec.C > maxExtraC {
					maxExtraC = ec.C
				}
			}
		}
		lineExtraSels = lineExtraSels[:0]
		if hasExtraSel {
			for k, ec := range b.extraCursors {
				anchor := b.extraSelAnchors[k]
				if sameCaret(anchor, ec) { // TargetX-only difference is not a selection
					continue
				}
				es, ee := normalizeRange(anchor, ec)
				if lineIdx >= es.L && lineIdx <= ee.L {
					lineExtraSels = append(lineExtraSels, [2]Loc{es, ee})
				}
			}
		}

		currentX := 0
		if len(lineData) == 0 && lineIdx == b.cursor.L {
			cursorVX = lineNumWidth - b.hOffset
			cursorVY = currentRenderY
		}
		// ponytail: render extra cursors on empty lines (an empty line can hold at
		// most one, since cleanExtraCursors dedupes by position)
		if len(lineData) == 0 && maxExtraC >= 0 {
			ex := lineNumWidth - b.hOffset
			if ex >= lineNumWidth && ex < w {
				setCell(ex, currentRenderY, ' ', nil, tcell.StyleDefault.Reverse(true))
			}
		}

		for i := vl.startCX; i < vl.endCX; {
			r, size := utf8.DecodeRune(lineData[i:])
			rw := fastRuneWidth(r, e.cfg.TabSize)

			if lineIdx == b.cursor.L && i == b.cursor.C {
				if !(b.stickToWrapEnd && i == vl.startCX && currSub > 0) { // 🟢 vIdx 불필요
					cursorVX = lineNumWidth + currentX - b.hOffset
					cursorVY = currentRenderY
				}
			}

			charStyle := lineStyle
			if len(lineMatches) > 0 {
				for _, m := range lineMatches {
					if i >= m.loc.C && i < m.loc.C+m.matchLen {
						charStyle = matchHighlightStyle
						break
					}
				}
			}

			if hasSel && cellInSelRange(lineIdx, i, selStart, selEnd) {
				charStyle = selectedStyle
			}

			// ponytail: extra-cursor selection highlighting -- each extra
			// caret can carry its own independent selection.
			for _, sel := range lineExtraSels {
				if cellInSelRange(lineIdx, i, sel[0], sel[1]) {
					charStyle = selectedStyle
					break
				}
			}

			// ponytail: extra cursor rendering — reverse-video block
			for _, c := range lineExtraCols {
				if c == i {
					charStyle = charStyle.Reverse(true)
					break
				}
			}

			if currentX+rw > b.hOffset && currentX < b.hOffset+textMaxWidth {
				if r == '\t' {
					for tx := 0; tx < rw; tx++ {
						if currentX+tx >= b.hOffset && currentX+tx < b.hOffset+textMaxWidth {
							setCell(lineNumWidth+currentX+tx-b.hOffset, currentRenderY, ' ', nil, charStyle)
						}
					}
				} else {
					drawX := lineNumWidth + currentX - b.hOffset
					if drawX < lineNumWidth {
						setCell(lineNumWidth, currentRenderY, ' ', nil, charStyle)
					} else {
						setCell(drawX, currentRenderY, r, nil, charStyle)
					}
				}
			}
			currentX += rw
			i += size
			// 🟢 [Phase 3-A] 가시영역 오른쪽을 완전히 벗어났고, 커서도 지났으면 중단
			// 커서가 이 줄이면 반드시 커서 위치를 지난 뒤에만 break → cursorVX/VY 누락 없음
			//
			// 보조 커서는 이 조건에 넣지 않는다. 여기서 break가 걸린 시점이면 이후 셀은
			// 전부 가시영역 밖이고, 아래 setCell들은 모두 가시영역 검사로 막혀 있어
			// 어차피 아무것도 그리지 않는다. 보조 커서까지 기다리게 하면 화면에 보이지도
			// 않는 수만 글자를 매 프레임 디코딩/스타일링하게 된다.
			// (wrap on/off × hOffset × 커서 위치 168개 조합에서 렌더 결과 동일함을 확인)
			if currentX-b.hOffset >= textMaxWidth && (lineIdx != b.cursor.L || i > b.cursor.C) {
				break
			}
		}

		// The newline cell at end-of-line. Uses the same cellInSelRange rules as the
		// in-line cells above (it used to duplicate them inline, which both invited
		// drift and ignored extra selections -- so a multi-line extra selection
		// rendered without its trailing newline highlighted, inconsistent with the
		// primary selection on the very same screen).
		if !vl.isWrapped || vl.endCX == len(lineData) {
			eol := len(lineData)
			selected := hasSel && cellInSelRange(lineIdx, eol, selStart, selEnd)
			if !selected {
				for _, sel := range lineExtraSels {
					if cellInSelRange(lineIdx, eol, sel[0], sel[1]) {
						selected = true
						break
					}
				}
			}
			if selected && currentX >= b.hOffset && currentX < b.hOffset+textMaxWidth {
				setCell(lineNumWidth+currentX-b.hOffset, currentRenderY, ' ', nil, selectedStyle)
			}
		}

		if lineIdx == b.cursor.L && b.cursor.C == vl.endCX {
			if b.cursor.C == len(lineData) || currSub+1 >= len(vcls) || b.stickToWrapEnd { // 🟢 vIdx 불필요
				cursorVX = lineNumWidth + currentX - b.hOffset
				cursorVY = currentRenderY
			}
		}

		// ponytail: render extra cursors at end-of-line
		if len(lineExtraCols) > 0 {
			for _, ecC := range lineExtraCols {
				if ecC == vl.endCX {
					if ecC == len(lineData) || currSub+1 >= len(vcls) {
						ex := lineNumWidth + currentX - b.hOffset
						if ex >= lineNumWidth && ex < w {
							setCell(ex, currentRenderY, ' ', nil, tcell.StyleDefault.Reverse(true))
						}
					}
				}
			}
		}

		vlWidth := vl.width

		showLeftIndicator := b.hOffset > 0 && vlWidth > 0
		showRightIndicator := vlWidth > b.hOffset+textMaxWidth

		if showLeftIndicator || showRightIndicator {
			indicatorStyle := lineStyle.Foreground(tcell.ColorDarkCyan).Bold(true)

			if showLeftIndicator {
				indX := lineNumWidth - 1
				if lineNumWidth <= 0 {
					indX = 0
				}
				if indX >= 0 && indX < w {
					mainc, _, _, _ := s.GetContent(indX, currentRenderY)
					if runewidth.RuneWidth(mainc) == 2 {
						setCell(indX, currentRenderY, ' ', nil, lineStyle)
						if indX+1 < w {
							setCell(indX+1, currentRenderY, ' ', nil, lineStyle)
						}
					}
				}
				setCell(indX, currentRenderY, '<', nil, indicatorStyle)
			}

			if showRightIndicator {
				if w-2 >= 0 {
					mainc, _, _, _ := s.GetContent(w-2, currentRenderY)
					if runewidth.RuneWidth(mainc) == 2 {
						setCell(w-2, currentRenderY, ' ', nil, lineStyle)
					}
				}
				if w-1 >= 0 {
					mainc, _, _, _ := s.GetContent(w-1, currentRenderY)
					if runewidth.RuneWidth(mainc) == 2 {
						setCell(w-1, currentRenderY, ' ', nil, lineStyle)
					}
				}
				setCell(w-1, currentRenderY, '>', nil, indicatorStyle)
			}
		}
		currentRenderY++
		currSub++
		if currSub >= len(vcls) {
			currSub = 0
			currL++
		}
	}

	for y := currentRenderY; y < h-1; y++ {
		for x := 0; x < w; x++ {
			setCell(x, y, ' ', nil, tcell.StyleDefault)
		}
	}

	if e.promptMode {
		var promptMsg string
		if e.promptType == "quit" {
			promptMsg = " [Warning] 저장되지 않은 탭이 있습니다. 무시하고 종료할까요? [Y/n]"
		} else if e.promptType == "close" {
			promptMsg = " [Warning] 변경된 내용이 있습니다. 탭을 닫을까요? [Y/n]"
		} else if e.promptType == "reset_config" {
			promptMsg = " [Warning] 설정을 기본값으로 초기화하시겠습니까? [Y/n]"
		} else if e.promptType == "reset_keybinds" {
			promptMsg = " [Warning] 모든 단축키를 기본값으로 되돌리시겠습니까? [Y/n]"
		} else if e.promptType == "reopen" {
			promptMsg = " [Warning] 변경된 내용이 있습니다. 무시하고 다시 열까요? [Y/n]"
		} else if e.promptType == "close_config" {
			promptMsg = " [Warning] 설정이 저장되지 않았습니다. 무시하고 닫을까요? [Y/n]"
		} else if e.promptType == "external_change" {
			fileName := ""
			if e.targetCloseBuffer >= 0 && e.targetCloseBuffer < len(e.buffers) {
				fileName = filepath.Base(e.buffers[e.targetCloseBuffer].filePath)
			}
			promptMsg = fmt.Sprintf(" [Warning] '%s' 파일이 외부에서 변경되었습니다. 디스크 내용으로 덮어쓸까요? [Y/n]", fileName)
		} else if e.promptType == "alert" {
			promptMsg = " [Alert] " + e.alertMessage + " (Enter/Esc)"
		}

		promptStyle := tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true)

		currentX := 0
		for _, r := range promptMsg {
			if currentX < w {
				setCell(currentX, h-1, r, nil, promptStyle)
				currentX += runewidth.RuneWidth(r)
			}
		}
		for currentX < w {
			setCell(currentX, h-1, ' ', nil, promptStyle)
			currentX++
		}
		cursorVX = runewidth.StringWidth(promptMsg)
		cursorVY = h - 1

	} else if b.searchMode || b.gotoMode {
		closeBtn := " [X] "
		b.closeBtnStartX = w - len(closeBtn)
		b.closeBtnEndX = w - 1

		checkboxArea := ""
		if !b.gotoMode {
			cbRegex := "[ ] .*"
			if b.searchRegex {
				cbRegex = "[x] .*"
			}
			cbCase := "[ ] Aa"
			if b.searchCase {
				cbCase = "[x] Aa"
			}
			cbWord := "[ ] \\b"
			if b.searchWord {
				cbWord = "[x] \\b"
			}
			checkboxArea = " " + cbRegex + "  " + cbCase + "  " + cbWord + " "
		}
		cbLen := runewidth.StringWidth(checkboxArea)
		cbStartX := b.closeBtnStartX - cbLen

		if !b.gotoMode {
			b.chkRegexX1 = cbStartX + 1
			b.chkRegexX2 = b.chkRegexX1 + 6
			b.chkCaseX1 = b.chkRegexX2 + 2
			b.chkCaseX2 = b.chkCaseX1 + 6
			b.chkWordX1 = b.chkCaseX2 + 2
			b.chkWordX2 = b.chkWordX1 + 6
		}

		var prefix, suffix string
		var targetStr *[]rune

		if b.gotoMode {
			prefix = " [Go To] Line,Col: "
			targetStr = &b.gotoInput
			suffix = "  (Enter: 이동, Esc: 취소)"
		} else if b.isReplace {
			if b.replaceStep == 1 {
				prefix = " [Replace] Find: "
				targetStr = &b.searchQuery
			} else if b.replaceStep == 2 {
				prefix = " [Replace] Find: " + string(b.searchQuery) + "  ➔ Replace: "
				targetStr = &b.replaceQuery
			} else {
				matchCountStr := "0/0"
				if len(b.matches) > 0 {
					totStr := sprintfRight(len(b.matches), 0)
					if b.searchCapped {
						totStr += "+"
					}
					matchCountStr = sprintfRight(b.matchIdx+1, 0) + "/" + totStr
				}
				prefix = " [Replace] Find: " + string(b.searchQuery) + "  ➔ Replace: " + string(b.replaceQuery) + "  [" + matchCountStr + "] (Enter:바꾸기, Up:이전, Down:건너뛰기, Ctrl+A:모두)"
				targetStr = nil
			}
		} else {
			matchCountStr := "0/0"
			if len(b.matches) > 0 {
				totStr := sprintfRight(len(b.matches), 0)
				if b.searchCapped {
					totStr += "+"
				}
				matchCountStr = sprintfRight(b.matchIdx+1, 0) + "/" + totStr
			}
			prefix = " [Find] Search: "
			targetStr = &b.searchQuery
			suffix = "  [" + matchCountStr + "] (Enter/Down:다음, Up:이전)"
		}

		currentX := 0
		for _, r := range prefix {
			if currentX < cbStartX {
				setCell(currentX, h-1, r, nil, statusStyle)
				currentX += runewidth.RuneWidth(r)
			}
		}

		inputStartX := currentX
		suffixW := runewidth.StringWidth(suffix)
		inputBoxEnd := cbStartX - suffixW
		if inputBoxEnd <= inputStartX {
			inputBoxEnd = inputStartX + 5 // fail-safe
		}

		if targetStr != nil {
			selStart, selEnd := b.inputSelStart, b.inputSelEnd
			if selStart > selEnd {
				selStart, selEnd = selEnd, selStart
			}

			totalW := runewidth.StringWidth(string(*targetStr))
			cW := runewidth.StringWidth(string((*targetStr)[:b.inputCX]))
			maxW := inputBoxEnd - inputStartX

			for {
				leftArrow := 0
				if b.inputHOffset > 0 {
					leftArrow = 1
				}
				rightArrow := 0
				if totalW-b.inputHOffset > maxW-leftArrow {
					rightArrow = 1
				}
				maxCursorW := maxW - 1 - leftArrow - rightArrow

				if cW < b.inputHOffset {
					b.inputHOffset = cW
					continue
				} else if cW-b.inputHOffset > maxCursorW {
					b.inputHOffset = cW - maxCursorW
					if b.inputHOffset < 0 {
						b.inputHOffset = 0
					}
					continue
				}
				break
			}

			leftArrowVal := 0
			if b.inputHOffset > 0 {
				leftArrowVal = 1
			}
			rightArrow := totalW-b.inputHOffset > maxW-leftArrowVal

			if leftArrowVal == 1 {
				setCell(currentX, h-1, '<', nil, tcell.StyleDefault.Foreground(tcell.ColorYellow).Bold(true))
				currentX++
			}

			limitX := inputBoxEnd
			if rightArrow {
				limitX = inputBoxEnd - 1
			}

			strW := 0
			for i, r := range *targetStr {
				rw := runewidth.RuneWidth(r)
				style := statusStyle
				if b.isInputSelect && i >= selStart && i < selEnd {
					style = selectedStyle
				}

				if strW >= b.inputHOffset && currentX < limitX {
					if currentX+rw > limitX {
						break
					}
					setCell(currentX, h-1, r, nil, style)
					currentX += rw
				}
				strW += rw
			}

			if rightArrow {
				setCell(inputBoxEnd-1, h-1, '>', nil, tcell.StyleDefault.Foreground(tcell.ColorYellow).Bold(true))
				currentX = inputBoxEnd
			}

			cursorVX = inputStartX + (cW - b.inputHOffset)
			if leftArrowVal == 1 {
				cursorVX++
			}
			if cursorVX >= inputBoxEnd {
				cursorVX = inputBoxEnd - 1
			}
		} else {
			cursorVX = -1
		}

		for _, r := range suffix {
			if currentX < cbStartX {
				setCell(currentX, h-1, r, nil, statusStyle)
				currentX += runewidth.RuneWidth(r)
			}
		}
		for currentX < cbStartX {
			setCell(currentX, h-1, ' ', nil, statusStyle)
			currentX++
		}
		cx := cbStartX
		for _, r := range checkboxArea {
			setCell(cx, h-1, r, nil, statusStyle)
			cx += runewidth.RuneWidth(r)
		}
		cursorVY = h - 1

		closeStyle := tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true)
		for i, r := range closeBtn {
			if b.closeBtnStartX+i < w {
				setCell(b.closeBtnStartX+i, h-1, r, nil, closeStyle)
			}
		}

	} else {
		modeName := b.encoding
		if b.isConfig {
			modeName = "CONFIG.JSON"
		}
		charCountStr := ""
		if hasSel {
			selChars := 0
			if selStart.L == selEnd.L {
				selChars = utf8.RuneCount(b.lines[selStart.L][selStart.C:selEnd.C])
			} else {
				selChars += utf8.RuneCount(b.lines[selStart.L][selStart.C:])
				for idx := selStart.L + 1; idx < selEnd.L; idx++ {
					selChars += utf8.RuneCount(b.lines[idx])
				}
				selChars += utf8.RuneCount(b.lines[selEnd.L][:selEnd.C])
				selChars += selEnd.L - selStart.L
			}
			charCountStr = fmt.Sprintf("%d Sel", selChars)
		} else {
			charCountStr = fmt.Sprintf("%d Chars", b.totalChars)
		}

		// 💡 리바인딩을 따라가도록 라이브 바인딩에서 생성한다.
		prefix := " Command Palette | "
		if sc := e.shortcutOf(actionIDPalette); sc != "" {
			prefix = " [" + sc + "] Command Palette | "
		}
		encodeStr := "Encode:" + modeName

		roStr := ""
		if b.isReadOnly {
			roStr = " | 🔒 READONLY"
		}

		displayPath := b.filePath
		if displayPath == "" {
			displayPath = "New Buffer"
		} else {
			pathRunes := []rune(displayPath)
			if len(pathRunes) > 50 {
				displayPath = "..." + string(pathRunes[len(pathRunes)-47:])
			}
		}

		runeCol := 1
		if b.cursor.L >= 0 && b.cursor.L < len(b.lines) {
			safeC := b.cursor.C
			if safeC > len(b.lines[b.cursor.L]) {
				safeC = len(b.lines[b.cursor.L])
			}
			runeCol = utf8.RuneCount(b.lines[b.cursor.L][:safeC]) + 1
		}
		suffix := fmt.Sprintf(" | %s | Ln %d, Col %d | %s%s ", charCountStr, b.cursor.L+1, runeCol, displayPath, roStr)

		currentX := 0
		for _, r := range prefix {
			if currentX < w {
				setCell(currentX, h-1, r, nil, statusStyle)
				currentX += runewidth.RuneWidth(r)
			}
		}
		b.encodeBtnX1 = currentX
		encodeStyle := tcell.StyleDefault.Background(tcell.ColorDarkCyan).Foreground(tcell.ColorWhite).Bold(true)
		for _, r := range encodeStr {
			if currentX < w {
				setCell(currentX, h-1, r, nil, encodeStyle)
				currentX += runewidth.RuneWidth(r)
			}
		}
		b.encodeBtnX2 = currentX - 1
		for _, r := range suffix {
			if currentX < w {
				setCell(currentX, h-1, r, nil, statusStyle)
				currentX += runewidth.RuneWidth(r)
			}
		}

		for currentX < w {
			setCell(currentX, h-1, ' ', nil, statusStyle)
			currentX++
		}
	}

	// 💡 centered=true 면 클릭 지점이 아니라 화면 중앙에 띄운다 (팔레트, 단축키 설정).
	drawMenu := func(isActive bool, title string, items []PaletteItem, cursor, anchorX, anchorY int, centered bool, outX, outY, outW, outH *int) {
		if !isActive {
			return
		}
		pWidth := 40
		if centered {
			pWidth = 60
		}
		for _, item := range items {
			// 💡 이름과 단축키가 한 줄에 좌/우로 나뉘어 그려지므로 둘 다 폭에 반영한다.
			// (단축키 설정 메뉴는 양쪽이 다 길어서, 이름만 재면 서로 겹쳐 보인다)
			w := runewidth.StringWidth(item.Name) + runewidth.StringWidth(item.Shortcut) + 6
			if w > pWidth {
				pWidth = w
			}
		}
		if pWidth > w-2 {
			pWidth = w - 2
		}
		pHeight := len(items) + 2
		if pHeight > h-4 {
			pHeight = h - 4
		}
		if pWidth < 4 || pHeight < 3 {
			return
		}

		pX, pY := anchorX, anchorY
		if centered {
			pX = (w - pWidth) / 2
			pY = (h - pHeight) / 2
		} else if *outW == 0 && *outH == 0 {
			// 💡 컨텍스트 메뉴 및 인코딩 메뉴의 스마트 포지셔닝
			// 클릭 지점(anchorX, anchorY) 기준 우측 아래(x+1, y+1)로 배치하되, 화면 경계를 넘어가면 반대 방향으로 뒤집음

			// Y축: 아래쪽 공간이 충분하면 아래로(anchorY + 1), 부족하면 위로(anchorY - pHeight)
			if anchorY+1+pHeight <= h-1 {
				pY = anchorY + 1
			} else {
				pY = anchorY - pHeight
				if pY < e.tabHeight { // 위로 올렸는데 탭 바를 넘어가면 아래쪽으로 붙임
					pY = e.tabHeight
				}
			}

			// X축: 오른쪽 공간이 충분하면 오른쪽으로(anchorX + 1), 부족하면 왼쪽으로(anchorX - pWidth)
			if anchorX+1+pWidth <= w {
				pX = anchorX + 1
			} else {
				pX = anchorX - pWidth
				if pX < 0 { // 왼쪽으로 밀었는데 화면 밖으로 나가면 화면 왼쪽 끝(0)에 붙임
					pX = 0
				}
			}
		} else {
			// 이미 계산된 위치가 있다면 화면 경계 밖으로 안 나가게 일반 클램핑만 수행
			if pX < 0 {
				pX = 0
			}
			if pY < 0 {
				pY = 0
			}
			if pX+pWidth > w {
				pX = w - pWidth
			}
			if pY+pHeight > h {
				pY = h - pHeight
			}
		}
		*outX, *outY, *outW, *outH = pX, pY, pWidth, pHeight

		// 💡 [개선] 기존의 사방 1칸 공백(마진) 제거는 화면에 '구멍'을 뚫어 버그처럼 보이고 커서를 가렸습니다.
		// 대신, 메뉴 왼쪽 경계(pX-1)에 한글 등 2칸 차지하는 더블바이트 문자가 있어 메뉴 안쪽(pX)으로
		// 깨져서 침범(Bleeding)하는 현상만 정밀 조준하여 해당 칸만 공백으로 클리어해 줍니다.
		for y := 0; y < pHeight; y++ {
			ly := pY + y
			lx := pX - 1
			if lx >= 0 && lx < w && ly >= 0 && ly < h {
				mainc, _, _, width := s.GetContent(lx, ly)
				if width == 2 || runewidth.RuneWidth(mainc) == 2 {
					setCell(lx, ly, ' ', nil, tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorDefault))
				}
			}
		}

		borderStyle := tcell.StyleDefault.Background(tcell.ColorDarkBlue).Foreground(tcell.ColorWhite)
		itemStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite)
		selectedStyle := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack).Bold(true)

		visibleItems := pHeight - 2

		// 💡 오프셋이 범위를 벗어나지 않도록 클램핑
		if e.menuScrollOffset < 0 {
			e.menuScrollOffset = 0
		}
		maxOffset := len(items) - visibleItems
		if maxOffset < 0 {
			maxOffset = 0
		}
		if e.menuScrollOffset > maxOffset {
			e.menuScrollOffset = maxOffset
		}
		startIdx := e.menuScrollOffset

		for y := 0; y < pHeight; y++ {
			for x := 0; x < pWidth; x++ {
				style := itemStyle
				r := ' '
				if y == 0 || y == pHeight-1 || x == 0 || x == pWidth-1 {
					style = borderStyle
					if y == 0 && x > 0 && x < pWidth-1 {
						r = '─'
						// 💡 상단 스크롤 화살표 표시
						if startIdx > 0 && x == pWidth-3 {
							r = '▲'
							style = borderStyle.Foreground(tcell.ColorYellow).Bold(true)
						}
					}
					if y == pHeight-1 && x > 0 && x < pWidth-1 {
						r = '─'
						// 💡 하단 스크롤 화살표 표시
						if startIdx+visibleItems < len(items) && x == pWidth-3 {
							r = '▼'
							style = borderStyle.Foreground(tcell.ColorYellow).Bold(true)
						}
					}
					if x == 0 && y > 0 && y < pHeight-1 {
						r = '│'
					}
					if x == pWidth-1 && y > 0 && y < pHeight-1 {
						r = '│'
					}
					if x == 0 && y == 0 {
						r = '┌'
					}
					if x == pWidth-1 && y == 0 {
						r = '┐'
					}
					if x == 0 && y == pHeight-1 {
						r = '└'
					}
					if x == pWidth-1 && y == pHeight-1 {
						r = '┘'
					}
				}
				setCell(pX+x, pY+y, r, nil, style)
			}
		}

		if title != "" {
			tx := pX + (pWidth-runewidth.StringWidth(title))/2
			// 💡 셀 폭만큼 전진해야 한다. range 의 인덱스는 바이트 오프셋이라
			// 한글처럼 2칸짜리 문자가 섞이면 글자 사이로 테두리(─)가 비집고 나온다.
			for _, r := range title {
				setCell(tx, pY, r, nil, borderStyle)
				tx += runewidth.RuneWidth(r)
			}
		}

		for i := 0; i < visibleItems && startIdx+i < len(items); i++ {
			idx := startIdx + i
			item := items[idx]
			style := itemStyle
			if idx == cursor {
				style = selectedStyle
			}

			for x := 1; x < pWidth-1; x++ {
				setCell(pX+x, pY+1+i, ' ', nil, style)
			}

			strName := " " + item.Name
			strShortcut := item.Shortcut + " "
			cx := pX + 1
			for _, r := range strName {
				setCell(cx, pY+1+i, r, nil, style)
				cx += runewidth.RuneWidth(r)
			}
			scLen := runewidth.StringWidth(strShortcut)
			cx = pX + pWidth - 1 - scLen
			for _, r := range strShortcut {
				setCell(cx, pY+1+i, r, nil, style)
				cx += runewidth.RuneWidth(r)
			}
		}
		if centered {
			cursorVX = -1
		}
	}

	drawMenu(e.paletteActive, " Command Palette ", e.paletteItems, e.paletteCursor, 0, 0, true, &e.paletteX, &e.paletteY, &e.paletteW, &e.paletteH)
	drawMenu(e.ctxMenuActive, "", e.ctxMenuItems, e.ctxMenuCursor, e.ctxMenuX, e.ctxMenuY, false, &e.ctxMenuX, &e.ctxMenuY, &e.ctxMenuW, &e.ctxMenuH)
	drawMenu(e.encodeMenuActive, e.encodeMenuTitle, e.encodeMenuItems, e.encodeMenuCursor, e.encodeMenuX, e.encodeMenuY, false, &e.encodeMenuX, &e.encodeMenuY, &e.encodeMenuW, &e.encodeMenuH)
	drawMenu(e.keyMenuActive, e.keyMenuTitle, e.keyMenuItems, e.keyMenuCursor, e.keyMenuX, e.keyMenuY, true, &e.keyMenuX, &e.keyMenuY, &e.keyMenuW, &e.keyMenuH)

	e.prevActiveBuf = e.activeBuffer
	e.prevVOffset = b.vOffsetL
	e.prevVOffsetSub = b.vOffsetSub
	e.prevHOffset = b.hOffset
	e.prevPalette = e.paletteActive
	e.prevCtxMenu = e.ctxMenuActive
	e.prevEncode = e.encodeMenuActive
	e.prevKeyMenu = e.keyMenuActive
	e.prevPrompt = e.promptMode
	e.prevSearch = b.searchMode
	e.prevGoto = b.gotoMode
	e.prevReplace = b.isReplace
	e.prevLinesLen = len(b.lines)

	e.prevCursor = b.cursor
	e.prevSelStart = b.selection.Start
	e.prevSelEnd = b.selection.End
	e.prevTotalChars = b.totalChars
	e.prevTxID = b.txIDCounter
	e.prevIsSelecting = b.isSelecting
	// append onto the retained backing arrays: allocation-free after the first frame
	e.prevExtraCursors = append(e.prevExtraCursors[:0], b.extraCursors...)
	e.prevExtraSelAnchors = append(e.prevExtraSelAnchors[:0], b.extraSelAnchors...)

	b.dirtyStartL = -1

	if cursorVX >= lineNumWidth && cursorVX < w && cursorVY >= 0 && cursorVY < h {
		s.ShowCursor(cursorVX, cursorVY)
	} else {
		s.HideCursor()
	}
	s.Show()
}

var globalScreenHandle *tcell.Screen
var shuttingDown atomic.Bool

func (e *Editor) openFile(s tcell.Screen) {
	s.Suspend()
	filePath, err := zenity.SelectFile(zenity.Title("파일 열기"))
	s.Resume()

	// 💡 FIX: GUI 창이 닫힌 직후 터미널 화면 강제 전체 리프레시!
	s.Sync()
	e.needsFullRefresh = true

	if err != nil || filePath == "" {
		return
	}

	// 💡 failsafe: 대용량 파일 경고
	fileInfo, errStat := os.Stat(filePath)
	if errStat == nil && fileInfo.Size() > 50*1024*1024 {
		s.Suspend()
		errConfirm := zenity.Question(
			fmt.Sprintf("파일 크기가 매우 큽니다 (%.1f MB).\n열면 속도가 느려지거나 멈출 수 있습니다. 계속 진행하시겠습니까?", float64(fileInfo.Size())/(1024*1024)),
			zenity.Title("대용량 파일 경고"),
			zenity.OKLabel("예"),
			zenity.CancelLabel("아니오"),
		)
		s.Resume()
		s.Sync()
		e.needsFullRefresh = true
		if errConfirm != nil {
			return
		}
	}

	lines, encoding, totalChars, hash, endsWithNewline, err := loadFileLines(filePath, "")
	if err != nil {
		return
	}

	b := NewBuffer()
	b.filePath = filePath
	b.lines = lines
	b.encoding = encoding

	// 💡 파일 감지기에 등록
	e.fileWatcher.Add(filePath)

	b.vCache = make(map[int][]VisualLine)
	b.vLinesValid = false
	b.totalChars = totalChars
	b.savedTotalChars = totalChars
	b.currentHash = hash
	b.savedHash = hash
	b.endsWithNewline = endsWithNewline
	b.savedStrongHash = b.computeStrongHash()
	b.updateSavedFileInfo()
	e.buffers = append(e.buffers, b)
	e.activeBuffer = len(e.buffers) - 1
}

func (e *Editor) saveActiveFile(s tcell.Screen) {
	b := e.getActive()
	if b.filePath == "" && !b.isConfig {
		e.saveAsFile(s)
		return
	}

	err := b.saveToFile(b.filePath)
	if err != nil {
		e.promptMode = true
		e.promptType = "alert"
		e.alertMessage = fmt.Sprintf("저장 실패: %v", err)
		return // 💡 실패하면 저장되었다고 마킹하지 않고 빠져나감
	}

	b.markSaved()

	if b.isConfig {
		content := b.getContent()
		var newCfg Config
		if err := json.Unmarshal([]byte(content), &newCfg); err == nil {
			e.applyConfig(newCfg)
		}
	}
}

func (e *Editor) saveAsFile(s tcell.Screen) {
	b := e.getActive()
	s.Suspend()
	filePath, err := zenity.SelectFileSave(zenity.Title("다른 이름으로 저장"), zenity.ConfirmOverwrite())
	s.Resume()

	s.Sync()
	e.needsFullRefresh = true

	if err != nil || filePath == "" {
		return
	}

	err = b.saveToFile(filePath)
	if err != nil {
		e.promptMode = true
		e.promptType = "alert"
		e.alertMessage = fmt.Sprintf("저장 실패: %v", err)
		return
	}

	// 💡 저장이 디스크에 무사히 완료된 후에만 내부 상태를 갱신합니다.
	oldPath := b.filePath
	b.filePath = filePath

	b.markSaved()

	// 💡 이름이 바뀌었다면 이전 파일의 감시를 해제하고 새 파일을 감시
	if oldPath != "" && oldPath != filePath {
		e.fileWatcher.Remove(oldPath)
	}
	e.fileWatcher.Add(filePath)

	if b.isConfig {
		content := b.getContent()
		var newCfg Config
		if err := json.Unmarshal([]byte(content), &newCfg); err == nil {
			e.applyConfig(newCfg)
		}
	}
}

func (e *Editor) toggleConfigBuffer() {
	for i, buf := range e.buffers {
		if buf.isConfig {
			// 💡 FIX 3: config.json이 수정된 상태라면 닫기 전에 경고창을 띄움
			if buf.isModified {
				e.promptMode = true
				e.promptType = "close_config"
				e.targetCloseBuffer = i
				return
			}
			e.closeBuffer(i)
			e.activeBuffer = 0
			return
		}
	}
	path := getConfigPath()
	if path == "" {
		return
	}
	lines, encoding, totalChars, hash, endsWithNewline, err := loadFileLines(path, "")
	if err != nil {
		return
	}

	configBuf := NewBuffer()
	configBuf.filePath = path
	configBuf.isConfig = true

	// 💡 config.json 파일도 감지기에 등록
	e.fileWatcher.Add(path)

	configBuf.lines = lines
	configBuf.encoding = encoding
	configBuf.vCache = make(map[int][]VisualLine)
	configBuf.vLinesValid = false
	configBuf.totalChars = totalChars
	configBuf.savedTotalChars = totalChars
	configBuf.currentHash = hash
	configBuf.savedHash = hash
	configBuf.endsWithNewline = endsWithNewline
	configBuf.savedStrongHash = configBuf.computeStrongHash()
	configBuf.updateSavedFileInfo()
	e.buffers = append(e.buffers, configBuf)
	e.activeBuffer = len(e.buffers) - 1
}

// --- [ 5. 검색 및 치환 로직 ] ---

// 💡 거리 계산용 헬퍼 함수
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// 💡 이진 탐색(O(log N))을 이용해 절대 거리가 가장 가까운 매치 인덱스를 찾는 함수
func (b *Buffer) findInitialMatchIdx(refLoc Loc, backwards bool) int {
	if len(b.matches) == 0 {
		return -1
	}

	// 1차 이진 탐색: 커서 위치보다 크거나 같은 첫 번째 결과 찾기
	idx := sort.Search(len(b.matches), func(i int) bool {
		m := b.matches[i]
		return m.loc.L > refLoc.L || (m.loc.L == refLoc.L && m.loc.C >= refLoc.C)
	})

	// 💡 처음 검색을 실행할 때 (matchIdx == -1), 위아래를 불문하고 '절대 거리'가 가장 가까운 곳으로 점프!
	if b.matchIdx == -1 && !backwards {
		if idx == 0 {
			return 0
		}
		if idx == len(b.matches) {
			return len(b.matches) - 1
		}

		// 커서 바로 앞의 매치(m1)와 바로 뒤의 매치(m2) 거리를 비교
		m1 := b.matches[idx-1]
		m2 := b.matches[idx]

		// 라인 거리에 가중치(10000)를 주어 계산 (위아래 줄이 같은 줄의 먼 글자보다 더 멀다고 판단)
		dist1 := absInt(refLoc.L-m1.loc.L)*10000 + absInt(refLoc.C-m1.loc.C)
		dist2 := absInt(m2.loc.L-refLoc.L)*10000 + absInt(m2.loc.C-refLoc.C)

		if dist1 <= dist2 {
			return idx - 1 // 위쪽이 더 가깝거나 거리가 같으면 위로 점프
		}
		return idx // 아래쪽이 더 가까우면 아래로 점프
	}

	// 기존 로직: Enter 누르면 다음으로, Shift+Enter 누르면 이전으로
	if backwards {
		if idx-1 >= 0 {
			return idx - 1
		}
		return len(b.matches) - 1
	}
	if idx < len(b.matches) {
		return idx
	}
	return 0
}

// preprocessReplaceTemplate — replaceQuery 내의 \n, \t 리터럴을 실제 제어 문자로 변환
func preprocessReplaceTemplate(replaceQuery []rune) (string, []byte) {
	s := string(replaceQuery)
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	return s, []byte(s)
}

// getSearchRegex — searchRegex/searchWord/searchCase 옵션에 따라 정규식을 컴파일
func (b *Buffer) getSearchRegex() *regexp.Regexp {
	query := string(b.searchQuery)
	if query == "" {
		return nil
	}
	if !b.searchRegex && !b.searchWord {
		return nil
	}
	pattern := query
	if !b.searchRegex {
		pattern = regexp.QuoteMeta(query)
	}
	if b.searchWord {
		pattern = `\b` + pattern + `\b`
	}
	if !b.searchCase {
		pattern = "(?i)" + pattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return compiled
}

// 💡 O(N) 전체 탐색을 버리고 커서 기준 위/아래 양방향(O(K))으로 뻗어나가는 극한 최적화 검색 엔진
func (b *Buffer) findAllMatches(overlap bool) {
	b.matches = []MatchInfo{}
	b.matchIdx = -1
	b.searchCapped = false
	query := string(b.searchQuery)
	if query == "" {
		b.clearSelection()
		return
	}

	re := b.getSearchRegex()
	if re == nil && (b.searchRegex || b.searchWord) && query != "" {
		b.matches = nil
		b.clearSelection()
		return
	}

	searchRunes := []rune(query)
	if re == nil && !b.searchCase {
		for i := range searchRunes {
			searchRunes[i] = unicode.ToLower(searchRunes[i])
		}
	}

	// 💡 1줄 안에서 매치를 찾는 공통 로직 (클로저)
	findInLine := func(lineIdx int, limit int) []MatchInfo {
		if limit <= 0 {
			return nil
		}
		var lineMatches []MatchInfo
		lineData := b.lines[lineIdx]
		if re != nil {
			locs := re.FindAllSubmatchIndex(lineData, limit)
			for _, sloc := range locs {
				lineMatches = append(lineMatches, MatchInfo{
					loc:        Loc{L: lineIdx, C: sloc[0], TargetX: -1},
					matchLen:   sloc[1] - sloc[0],
					submatches: sloc,
				})
			}
		} else {
			offset := 0
			for offset <= len(lineData) {
				if len(lineMatches) >= limit {
					break
				}
				match := true
				currentByteOffset := offset
				for j := 0; j < len(searchRunes); j++ {
					if currentByteOffset >= len(lineData) {
						match = false
						break
					}
					tr, tsize := utf8.DecodeRune(lineData[currentByteOffset:])
					sr := searchRunes[j]
					if !b.searchCase {
						tr = unicode.ToLower(tr)
					}
					if tr != sr {
						match = false
						break
					}
					currentByteOffset += tsize
				}
				if match {
					matchLen := currentByteOffset - offset
					lineMatches = append(lineMatches, MatchInfo{loc: Loc{L: lineIdx, C: offset, TargetX: -1}, matchLen: matchLen})
					if !overlap {
						offset = currentByteOffset
					} else {
						_, size := utf8.DecodeRune(lineData[offset:])
						offset += size
					}
				} else {
					if offset >= len(lineData) {
						break
					}
					_, size := utf8.DecodeRune(lineData[offset:])
					offset += size
				}
			}
		}
		return lineMatches
	}

	var matchesAbove [][]MatchInfo
	var matchesBelow []MatchInfo
	countAbove, countBelow := 0, 0

	// 1. 현재 커서가 있는 줄 처리 (커서 앞/뒤 분리)
	cursorMatches := findInLine(b.cursor.L, 5000)
	var cursorLineAbove []MatchInfo
	for _, m := range cursorMatches {
		if m.loc.C < b.cursor.C {
			if countAbove < 5000 {
				cursorLineAbove = append(cursorLineAbove, m)
				countAbove++
			}
		} else {
			if countBelow < 5000 {
				matchesBelow = append(matchesBelow, m)
				countBelow++
			}
		}
	}
	if len(cursorLineAbove) > 0 {
		matchesAbove = append(matchesAbove, cursorLineAbove)
	}

	// 2. 아래 방향 탐색 (O(K))
	for i := b.cursor.L + 1; i < len(b.lines); i++ {
		if countBelow >= 5000 {
			b.searchCapped = true
			break
		}
		lm := findInLine(i, 5000-countBelow)
		if len(lm) > 0 {
			matchesBelow = append(matchesBelow, lm...)
			countBelow += len(lm)
		}
	}

	// 3. 위 방향 탐색 (O(K))
	for i := b.cursor.L - 1; i >= 0; i-- {
		if countAbove >= 5000 {
			b.searchCapped = true
			break
		}
		lm := findInLine(i, 5000-countAbove)
		if len(lm) > 0 {
			matchesAbove = append(matchesAbove, lm)
			countAbove += len(lm)
		}
	}

	// 4. 결과 병합 (정렬 상태 유지하며 O(1) Copy)
	b.matches = make([]MatchInfo, 0, countAbove+countBelow)
	for i := len(matchesAbove) - 1; i >= 0; i-- {
		b.matches = append(b.matches, matchesAbove[i]...)
	}
	b.matches = append(b.matches, matchesBelow...)

	b.clearSelection()
}

func (b *Buffer) jumpToMatch() {
	if b.matchIdx < 0 || b.matchIdx >= len(b.matches) {
		return
	}
	m := b.matches[b.matchIdx]
	b.cursor = m.loc
	b.isSelecting = true
	b.selection.Start = m.loc
	b.selection.End = Loc{L: m.loc.L, C: m.loc.C + m.matchLen, TargetX: -1}
}

func (b *Buffer) replaceCurrent(overlap bool) {
	if b.matchIdx < 0 || b.matchIdx >= len(b.matches) {
		return
	}
	m := b.matches[b.matchIdx]

	templateStr, templateBytes := preprocessReplaceTemplate(b.replaceQuery)
	replaceStr := templateStr

	re := b.getSearchRegex()
	if re != nil && len(m.submatches) > 0 {
		lineData := b.lines[m.loc.L]
		expanded := re.Expand(nil, templateBytes, lineData, m.submatches)
		replaceStr = string(expanded)
	}

	b.BeginTransaction()
	delEnd := Loc{L: m.loc.L, C: m.loc.C + m.matchLen, TargetX: -1}
	b.DeleteTextWithRecord(m.loc, delEnd)
	b.InsertTextWithRecord(m.loc, replaceStr)
	b.rebaseCaretsForEdit(m.loc, delEnd, m.loc, b.cursor)
	b.cleanExtraCursors()
	b.EndTransaction()

	targetLoc := b.cursor
	b.clearSelection()
	b.findAllMatches(overlap)

	if len(b.matches) > 0 {
		b.matchIdx = b.findInitialMatchIdx(targetLoc, false)
		b.jumpToMatch()
	} else {
		b.matchIdx = -1
	}
}

// --- [ 단축키 바인딩 엔진 ] ---

type EditorAction func(e *Editor, s tcell.Screen)

// 💡 KeyChord 는 "사용자가 실제로 누른 조합" 하나를 표현한다.
// tcell 이 조합을 세 가지 다른 모양으로 넘겨주기 때문에 정규화가 핵심이다.
//   - Ctrl+글자  -> KeyCtrlA..KeyCtrlZ (65..90) 상수 + ModCtrl + ch='a'..'z'
//   - Alt+글자   -> KeyRune + ModAlt + Rune
//   - Ctrl+Alt+Up -> KeyUp + ModCtrl|ModAlt
//
// 정규화하지 않으면 같은 물리 조합이 여러 chord 로 갈라져 맵 조회가 빗나간다.
type KeyChord struct {
	Key  tcell.Key
	Rune rune          // Key == tcell.KeyRune 일 때만 유효
	Mods tcell.ModMask // 정규화 후 ModCtrl|ModAlt|ModShift 만 남음
}

// 💡 Ctrl 이 상수에 이미 녹아 있는 키 구간 (tcell: KeyCtrlSpace=64 .. KeyCtrlUnderscore=95).
func isCtrlConstKey(k tcell.Key) bool {
	return k >= tcell.KeyCtrlSpace && k <= tcell.KeyCtrlUnderscore
}

func normalizeChord(c KeyChord) KeyChord {
	// KeyCtrlS 같은 상수도 ch 에 's' 를 실어 오므로 반드시 지운다. 안 지우면
	// 같은 키가 Rune 값에 따라 다른 맵 엔트리가 된다.
	if c.Key != tcell.KeyRune {
		c.Rune = 0
	}
	// Ctrl 이 상수에 접혀 있는 구간에서는 ModCtrl 을 떼어낸다 (중복 표현 제거).
	if isCtrlConstKey(c.Key) {
		c.Mods &^= tcell.ModCtrl
	}
	c.Mods &= tcell.ModCtrl | tcell.ModAlt | tcell.ModShift
	return c
}

func chordFromEvent(ev *tcell.EventKey) KeyChord {
	return normalizeChord(KeyChord{Key: ev.Key(), Rune: ev.Rune(), Mods: ev.Modifiers()})
}

func (c KeyChord) isZero() bool { return c.Key == 0 && c.Rune == 0 && c.Mods == 0 }

// 💡 tcell.KeyNames 는 "Ctrl-A" 하이픈 표기를 쓴다. 이 에디터 UI 는 예전부터
// "Ctrl+A" 표기이므로 Ctrl 구간만 자체 표를 쓴다.
var ctrlChordNames = map[tcell.Key]string{
	tcell.KeyCtrlSpace:      "Ctrl+Space",
	tcell.KeyCtrlA:          "Ctrl+A",
	tcell.KeyCtrlB:          "Ctrl+B",
	tcell.KeyCtrlC:          "Ctrl+C",
	tcell.KeyCtrlD:          "Ctrl+D",
	tcell.KeyCtrlE:          "Ctrl+E",
	tcell.KeyCtrlF:          "Ctrl+F",
	tcell.KeyCtrlG:          "Ctrl+G",
	tcell.KeyCtrlH:          "Ctrl+H",
	tcell.KeyCtrlI:          "Ctrl+I",
	tcell.KeyCtrlJ:          "Ctrl+J",
	tcell.KeyCtrlK:          "Ctrl+K",
	tcell.KeyCtrlL:          "Ctrl+L",
	tcell.KeyCtrlM:          "Ctrl+M",
	tcell.KeyCtrlN:          "Ctrl+N",
	tcell.KeyCtrlO:          "Ctrl+O",
	tcell.KeyCtrlP:          "Ctrl+P",
	tcell.KeyCtrlQ:          "Ctrl+Q",
	tcell.KeyCtrlR:          "Ctrl+R",
	tcell.KeyCtrlS:          "Ctrl+S",
	tcell.KeyCtrlT:          "Ctrl+T",
	tcell.KeyCtrlU:          "Ctrl+U",
	tcell.KeyCtrlV:          "Ctrl+V",
	tcell.KeyCtrlW:          "Ctrl+W",
	tcell.KeyCtrlX:          "Ctrl+X",
	tcell.KeyCtrlY:          "Ctrl+Y",
	tcell.KeyCtrlZ:          "Ctrl+Z",
	tcell.KeyCtrlLeftSq:     "Ctrl+[",
	tcell.KeyCtrlBackslash:  "Ctrl+\\",
	tcell.KeyCtrlRightSq:    "Ctrl+]",
	tcell.KeyCtrlCarat:      "Ctrl+^",
	tcell.KeyCtrlUnderscore: "Ctrl+_",
}

// 💡 "Ctrl+A" / "Up" / "F12" -> tcell.Key 역방향 표. init() 에서 한 번만 만든다.
var chordNameToKey map[string]tcell.Key

func init() {
	chordNameToKey = make(map[string]tcell.Key, len(tcell.KeyNames)+len(ctrlChordNames))
	for k, name := range tcell.KeyNames {
		if strings.HasPrefix(name, "Ctrl-") {
			continue // Ctrl 구간은 아래 자체 표가 담당
		}
		chordNameToKey[name] = k
	}
	for k, name := range ctrlChordNames {
		chordNameToKey[name] = k
	}
}

// 💡 Ctrl 구간이 아닌 키의 표시 이름. Backspace/Tab/Enter/Esc 처럼
// tcell.KeyNames 가 편집용 이름을 가진 키가 우선한다.
func keyBaseName(k tcell.Key) (string, bool) {
	if n, ok := tcell.KeyNames[k]; ok && !strings.HasPrefix(n, "Ctrl-") {
		return n, true
	}
	if n, ok := ctrlChordNames[k]; ok {
		return n, true
	}
	return "", false
}

// chordString 은 chord 를 "Ctrl+S" / "Alt+." / "Ctrl+Alt+Up" / "F12" 로 직렬화한다.
// 빈 chord(바인딩 없음)는 빈 문자열. config.json 저장 형식이자 메뉴 표시 형식이다.
func chordString(c KeyChord) string {
	c = normalizeChord(c)
	if c.isZero() {
		return ""
	}
	prefix := ""
	if c.Mods&tcell.ModCtrl != 0 {
		prefix += "Ctrl+"
	}
	if c.Mods&tcell.ModAlt != 0 {
		prefix += "Alt+"
	}
	if c.Mods&tcell.ModShift != 0 {
		prefix += "Shift+"
	}
	if c.Key == tcell.KeyRune {
		if c.Rune == ' ' {
			return prefix + "Space"
		}
		if c.Rune == 0 {
			return ""
		}
		return prefix + string(c.Rune)
	}
	name, ok := keyBaseName(c.Key)
	if !ok {
		return ""
	}
	return prefix + name
}

// parseChord 는 chordString 의 역연산. 빈 문자열은 "바인딩 해제"로 성공 처리한다.
// "Ctrl+A" 자체가 하나의 이름이므로 전체 문자열 조회를 먼저 시도하고,
// 실패했을 때만 앞쪽 수식어를 한 겹씩 벗긴다.
func parseChord(s string) (KeyChord, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return KeyChord{}, true
	}
	var mods tcell.ModMask
	for i := 0; i < 4; i++ {
		if k, ok := chordNameToKey[s]; ok {
			return normalizeChord(KeyChord{Key: k, Mods: mods}), true
		}
		if s == "Space" {
			return normalizeChord(KeyChord{Key: tcell.KeyRune, Rune: ' ', Mods: mods}), true
		}
		if utf8.RuneCountInString(s) == 1 {
			r, _ := utf8.DecodeRuneInString(s)
			return normalizeChord(KeyChord{Key: tcell.KeyRune, Rune: r, Mods: mods}), true
		}
		switch {
		case strings.HasPrefix(s, "Ctrl+"):
			mods |= tcell.ModCtrl
			s = s[len("Ctrl+"):]
		case strings.HasPrefix(s, "Alt+"):
			mods |= tcell.ModAlt
			s = s[len("Alt+"):]
		case strings.HasPrefix(s, "Shift+"):
			mods |= tcell.ModShift
			s = s[len("Shift+"):]
		default:
			return KeyChord{}, false
		}
	}
	return KeyChord{}, false
}

// 💡 최종 편집 switch 가 소유한 키들. 여기에 액션을 얹으면 타이핑/커서 이동이 죽는다.
// Alt 가 붙은 형태는 그 switch 에 닿지 않으므로 자유롭게 바인딩할 수 있다
// (Alt+Up/Alt+Down 이 실제로 줄 이동 기본값인 이유).
func reservedChord(c KeyChord) bool {
	c = normalizeChord(c)
	if c.isZero() {
		return true
	}
	if c.Mods&tcell.ModAlt != 0 {
		return false
	}
	switch c.Key {
	case tcell.KeyRune,
		tcell.KeyLeft, tcell.KeyRight, tcell.KeyUp, tcell.KeyDown,
		tcell.KeyHome, tcell.KeyEnd, tcell.KeyPgUp, tcell.KeyPgDn,
		tcell.KeyEnter, tcell.KeyTab, tcell.KeyBacktab,
		tcell.KeyBackspace, tcell.KeyBackspace2, tcell.KeyDelete,
		tcell.KeyEscape, tcell.KeyInsert:
		return true
	}
	return false
}

// ActionDef 는 리바인딩 가능한 액션 하나. ID 는 config.json 에 그대로 쓰이므로
// 한 번 정하면 바꾸지 않는다.
type ActionDef struct {
	ID   string
	Name string
	Def  KeyChord // 기본 바인딩
	Fn   EditorAction

	NoSnap   bool // 실행 후 snapToCursor = false (화면 점프 방지)
	Mutates  bool // isReadOnly 버퍼에서 차단
	PreModal bool // 팔레트/검색창/프롬프트보다 먼저 처리
}

const actionIDPalette = "palette"

func ctrlChord(k tcell.Key) KeyChord    { return KeyChord{Key: k} }
func altRuneChord(r rune) KeyChord      { return KeyChord{Key: tcell.KeyRune, Rune: r, Mods: tcell.ModAlt} }
func plainChord(k tcell.Key) KeyChord   { return KeyChord{Key: k} }
func altChord(k tcell.Key) KeyChord     { return KeyChord{Key: k, Mods: tcell.ModAlt} }
func ctrlAltChord(k tcell.Key) KeyChord { return KeyChord{Key: k, Mods: tcell.ModCtrl | tcell.ModAlt} }

// Actions 의 순서가 곧 단축키 설정 메뉴의 표시 순서다.
// 💡 var 초기화식이 아니라 init() 에서 채운다: 액션 클로저가 applyConfig ->
// initPalette -> showKeybindMenu -> Actions 로 되돌아와 초기화 사이클이 된다.
var Actions []ActionDef

var actionByID map[string]*ActionDef

func init() {
	Actions = []ActionDef{
		{ID: "open", Name: "파일 열기 (Open)", Def: ctrlChord(tcell.KeyCtrlO),
			Fn: func(e *Editor, s tcell.Screen) { e.openFile(s) }},

		{ID: "save", Name: "저장 (Save)", Def: ctrlChord(tcell.KeyCtrlS), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) { e.saveActiveFile(s) }},

		{ID: "save_as", Name: "다른 이름으로 저장 (Save As)", Def: plainChord(tcell.KeyF12), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) { e.saveAsFile(s) }},

		{ID: "new_tab", Name: "새 탭 열기 (New Tab)", Def: ctrlChord(tcell.KeyCtrlN), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				e.buffers = append(e.buffers, NewBuffer())
				e.activeBuffer = len(e.buffers) - 1
			}},

		{ID: "close_tab", Name: "현재 탭 닫기 (Close Tab)", Def: ctrlChord(tcell.KeyCtrlW), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				if b.isModified {
					e.promptMode = true
					e.promptType = "close"
					e.targetCloseBuffer = e.activeBuffer
					return
				}
				if len(e.buffers) <= 1 {
					shuttingDown.Store(true)
					s.Fini()
					os.Exit(0)
				}
				e.closeBuffer(e.activeBuffer)
				e.needsFullRefresh = true
			}},

		{ID: "next_tab", Name: "다음 탭 (Next Tab)", Def: altRuneChord('.'), NoSnap: true, PreModal: true,
			Fn: func(e *Editor, s tcell.Screen) {
				e.activeBuffer = (e.activeBuffer + 1) % len(e.buffers)
				e.needsFullRefresh = true
			}},

		{ID: "prev_tab", Name: "이전 탭 (Prev Tab)", Def: altRuneChord(','), NoSnap: true, PreModal: true,
			Fn: func(e *Editor, s tcell.Screen) {
				e.activeBuffer = (e.activeBuffer - 1 + len(e.buffers)) % len(e.buffers)
				e.needsFullRefresh = true
			}},

		{ID: "cycle_tab", Name: "탭 순환 (Cycle Tab)", Def: ctrlChord(tcell.KeyCtrlBackslash), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) { e.activeBuffer = (e.activeBuffer + 1) % len(e.buffers) }},

		{ID: "undo", Name: "실행 취소 (Undo)", Def: ctrlChord(tcell.KeyCtrlZ),
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().Undo() }},

		{ID: "redo", Name: "다시 실행 (Redo)", Def: ctrlChord(tcell.KeyCtrlY),
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().Redo() }},

		{ID: "select_all", Name: "모두 선택 (Select All)", Def: ctrlChord(tcell.KeyCtrlA), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().selectAll() }},

		{ID: "copy", Name: "복사 (Copy)", Def: ctrlChord(tcell.KeyCtrlC), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				// ponytail(CM6): copy every live selection (primary + each extra
				// cursor's own), not just the primary's -- see getMultiSelectedText.
				text := e.getActive().getMultiSelectedText()
				if text != "" {
					_ = clipboard.WriteAll(text)
				}
			}},

		{ID: "cut", Name: "잘라내기 (Cut)", Def: ctrlChord(tcell.KeyCtrlX),
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				if b.isReadOnly {
					return
				}
				text := b.getMultiSelectedText()
				if text != "" {
					_ = clipboard.WriteAll(text)
					b.BeginTransaction()
					b.DeleteSelection()
					b.EndTransaction()
				}
			}},

		{ID: "paste", Name: "붙여넣기 (Paste)", Def: ctrlChord(tcell.KeyCtrlV),
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				if b.isReadOnly {
					return
				}
				text, err := clipboard.ReadAll()
				if err == nil && text != "" {
					text = strings.ReplaceAll(text, "\r\n", "\n")
					b.BeginTransaction()
					b.DeleteSelection()
					// ponytail(CM6): if the clipboard splits into exactly one line per
					// live caret, give each caret its own line (in document order)
					// instead of pasting the whole blob at every caret -- mirrors
					// CodeMirror 6's "byLine" paste, the round-trip counterpart of
					// copying from N selections (getMultiSelectedText). Any other line
					// count falls back to the old "same text everywhere" paste, which
					// is also CM6's own fallback for a non-matching line count.
					cursorCount := 1 + len(b.extraCursors)
					parts := strings.Split(text, "\n")
					if cursorCount > 1 && len(parts) == cursorCount {
						rank := b.cursorRankMap()
						b.runMultiCursorInsert(func(loc Loc, _ bool) string { return parts[rank[loc]] })
					} else {
						b.runMultiCursorInsert(func(Loc, bool) string { return text })
					}
					b.EndTransaction()
				}
			}},

		{ID: "find", Name: "찾기 (Find)", Def: ctrlChord(tcell.KeyCtrlF), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				b.clearSelection()
				b.searchMode = true
				b.isReplace = false
				b.replaceStep = 0
				b.searchQuery = []rune{}
				b.replaceQuery = []rune{}
				b.matches = []MatchInfo{}
				b.matchIdx = -1
				b.inputCX = 0
				b.isInputSelect = false
			}},

		{ID: "replace", Name: "바꾸기 (Replace)", Def: ctrlChord(tcell.KeyCtrlR), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				b.clearSelection()
				b.searchMode = true
				b.isReplace = true
				b.replaceStep = 1
				b.searchQuery = []rune{}
				b.replaceQuery = []rune{}
				b.matches = []MatchInfo{}
				b.matchIdx = -1
				b.inputCX = 0
				b.isInputSelect = false
			}},

		{ID: "goto_line", Name: "줄 이동 (Go To Line)", Def: ctrlChord(tcell.KeyCtrlG), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				b.clearSelection()
				b.searchMode = false
				b.gotoMode = true
				b.gotoInput = []rune{}
				b.inputCX = 0
				b.isInputSelect = false
			}},

		{ID: "move_line_up", Name: "줄 위로 이동 (Move Line Up)", Def: altChord(tcell.KeyUp), Mutates: true,
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().moveLineUp() }},

		{ID: "move_line_down", Name: "줄 아래로 이동 (Move Line Down)", Def: altChord(tcell.KeyDown), Mutates: true,
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().moveLineDown() }},

		{ID: "add_cursor_above", Name: "위에 커서 추가 (Add Cursor Above)", Def: ctrlAltChord(tcell.KeyUp),
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().addCursorAbove(e.cfg) }},

		{ID: "add_cursor_below", Name: "아래에 커서 추가 (Add Cursor Below)", Def: ctrlAltChord(tcell.KeyDown),
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().addCursorBelow(e.cfg) }},

		{ID: "insert_time", Name: "시간 삽입 (Insert Time)", Def: plainChord(tcell.KeyF5),
			Fn: func(e *Editor, s tcell.Screen) {
				b := e.getActive()
				if b.isReadOnly {
					return
				}
				b.BeginTransaction()
				b.DeleteSelection()
				goLayout := convertLinuxDateToGoLayout(e.cfg.DateFormat)
				b.InsertTextWithRecord(b.cursor, time.Now().Format(goLayout))
				b.EndTransaction()
			}},

		{ID: actionIDPalette, Name: "커맨드 팔레트 (Command Palette)", Def: ctrlChord(tcell.KeyCtrlP), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				e.paletteActive = !e.paletteActive
				e.paletteCursor = 0
				e.menuScrollOffset = 0
				e.ctxMenuActive = false
				e.encodeMenuActive = false
				e.closeKeybindMenu()
			}},

		{ID: "toggle_config", Name: "설정 파일 편집 (Toggle Config)", Def: ctrlChord(tcell.KeyCtrlT), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) { e.getActive().clearSelection(); e.toggleConfigBuffer() }},

		{ID: "quit", Name: "에디터 종료 (Quit)", Def: ctrlChord(tcell.KeyCtrlQ), NoSnap: true,
			Fn: func(e *Editor, s tcell.Screen) {
				for _, b := range e.buffers {
					if b.isModified {
						e.promptMode = true
						e.promptType = "quit"
						return
					}
				}
				shuttingDown.Store(true)
				s.Fini()
				os.Exit(0)
			}},
	}

	actionByID = make(map[string]*ActionDef, len(Actions))
	for i := range Actions {
		actionByID[Actions[i].ID] = &Actions[i]
	}
}

// buildBindings 는 기본값 위에 cfg.Keybindings 오버라이드를 얹어 라이브 표를 만든다.
// config.json 은 손으로 편집할 수 있으므로 방어적으로 동작한다: 파싱 실패, 미지 ID,
// 예약 키는 조용히 무시하고 기본값을 유지한다.
//
// 배치는 반드시 "먼저 전부 비우고, 그 다음 전부 놓기" 두 단계여야 한다.
// 한 번에 하나씩 처리하면 두 액션이 서로의 키를 맞바꾼 설정(A=B의 기본값,
// B=A의 기본값)이 양쪽 다 충돌로 거부되어 통째로 기본값으로 되돌아간다.
func buildBindings(cfg Config) (map[KeyChord]*ActionDef, map[string]KeyChord) {
	byID := make(map[string]KeyChord, len(Actions))
	occupied := make(map[KeyChord]string, len(Actions))
	for i := range Actions {
		a := &Actions[i]
		c := normalizeChord(a.Def)
		byID[a.ID] = c
		if !c.isZero() {
			occupied[c] = a.ID
		}
	}

	// 맵 순회 순서가 결과를 바꾸지 않도록 ID 를 정렬해 적용한다.
	ids := make([]string, 0, len(cfg.Keybindings))
	for id := range cfg.Keybindings {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	type pendingBind struct {
		id    string
		chord KeyChord
	}
	pending := make([]pendingBind, 0, len(ids))
	for _, id := range ids {
		if _, known := actionByID[id]; !known {
			continue
		}
		chord, ok := parseChord(cfg.Keybindings[id])
		if !ok {
			continue
		}
		if chord.isZero() {
			if id == actionIDPalette {
				continue // 팔레트를 해제하면 되돌릴 길이 사라진다
			}
		} else if reservedChord(chord) {
			continue
		}
		pending = append(pending, pendingBind{id, chord})
	}

	// 1단계: 오버라이드 대상 액션들의 현재 점유를 모두 비운다 (자리 맞바꾸기 허용).
	for _, p := range pending {
		delete(occupied, byID[p.id])
		byID[p.id] = KeyChord{}
	}
	// 2단계: 배치. 남이 이미 쓰는 chord 면 무시한다.
	for _, p := range pending {
		if p.chord.isZero() {
			continue
		}
		if holder, dup := occupied[p.chord]; dup && holder != p.id {
			continue
		}
		byID[p.id] = p.chord
		occupied[p.chord] = p.id
	}
	// 3단계: 2단계에서 거부당한 액션은 기본값이 아직 비어 있으면 되돌려 준다.
	for _, p := range pending {
		if p.chord.isZero() || !byID[p.id].isZero() {
			continue
		}
		def := normalizeChord(actionByID[p.id].Def)
		if _, dup := occupied[def]; dup {
			continue
		}
		byID[p.id] = def
		occupied[def] = p.id
	}

	byChord := make(map[KeyChord]*ActionDef, len(byID))
	for id, c := range byID {
		if c.isZero() {
			continue
		}
		byChord[c] = actionByID[id]
	}
	return byChord, byID
}

// keybindOverrides 는 기본값과 다른 항목만 뽑아 config.json 에 기록할 맵을 만든다.
// 기본값과 같은 항목까지 쓰면 나중에 기본값을 바꿔도 사용자 파일이 옛 값을 붙잡는다.
func keybindOverrides(byID map[string]KeyChord) map[string]string {
	out := map[string]string{}
	for i := range Actions {
		a := &Actions[i]
		c, ok := byID[a.ID]
		if !ok {
			continue
		}
		if normalizeChord(c) == normalizeChord(a.Def) {
			continue
		}
		out[a.ID] = chordString(c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- [ CLI 엔진 및 파서 상태 ] ---
type StartupAction struct {
	Type     string // "file", "config", "picker"
	Path     string
	ReadOnly bool
}

func (e *Editor) openOrFocusFile(filePath string, isReadOnly bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	// 💡 추가 1: 열려는 대상이 디렉토리일 경우 경고 띄우고 취소
	if info, err := os.Stat(absPath); err == nil && info.IsDir() {
		e.promptMode = true
		e.promptType = "alert"
		e.alertMessage = fmt.Sprintf("'%s'은(는) 폴더입니다.", filepath.Base(absPath))
		return
	}

	// 💡 추가 2: 부모 디렉토리가 존재하지 않을 경우 경고 (일단 열어주긴 함)
	dir := filepath.Dir(absPath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		e.promptMode = true
		e.promptType = "alert"
		e.alertMessage = fmt.Sprintf("경고: 디렉토리가 존재하지 않습니다 (%s)", dir)
	}

	// 1. 이미 열린 탭인지 검사 (Focus & State Update)
	for i, buf := range e.buffers {
		if buf.filePath == absPath {
			e.activeBuffer = i
			buf.isReadOnly = isReadOnly // 💡 상태 덮어쓰기
			return
		}
	}

	// 💡 failsafe: 대용량 파일 경고
	fileInfo, errStat := os.Stat(absPath)
	if errStat == nil && fileInfo.Size() > 50*1024*1024 {
		if globalScreenHandle != nil && *globalScreenHandle != nil {
			(*globalScreenHandle).Suspend()
		}
		errConfirm := zenity.Question(
			fmt.Sprintf("파일 크기가 매우 큽니다 (%.1f MB).\n열면 속도가 느려지거나 멈출 수 있습니다. 계속 진행하시겠습니까?", float64(fileInfo.Size())/(1024*1024)),
			zenity.Title("대용량 파일 경고"),
			zenity.OKLabel("예"),
			zenity.CancelLabel("아니오"),
		)
		if globalScreenHandle != nil && *globalScreenHandle != nil {
			(*globalScreenHandle).Resume()
			(*globalScreenHandle).Sync()
		}
		e.needsFullRefresh = true
		if errConfirm != nil {
			return
		}
	}

	// 2. 새 탭으로 열기
	b := NewBuffer()
	b.filePath = absPath
	b.isReadOnly = isReadOnly
	lines, encoding, totalChars, hash, endsWithNewline, err := loadFileLines(absPath, "")
	if err == nil {
		b.lines = lines
		b.encoding = encoding
		e.fileWatcher.Add(absPath)
		b.vCache = make(map[int][]VisualLine)
		b.vLinesValid = false
		b.totalChars = totalChars
		b.savedTotalChars = totalChars
		b.currentHash = hash
		b.savedHash = hash
		b.endsWithNewline = endsWithNewline
		b.savedStrongHash = b.computeStrongHash()
		b.updateSavedFileInfo()
	}

	if !e.initialBufferUsed && len(e.buffers) == 1 && e.buffers[0].filePath == "" && !e.buffers[0].isModified && !e.buffers[0].isConfig {
		e.buffers[0] = b
		e.activeBuffer = 0
		e.initialBufferUsed = true
	} else {
		e.buffers = append(e.buffers, b)
		e.activeBuffer = len(e.buffers) - 1
	}
}

func (e *Editor) focusOrOpenConfig(isReadOnly bool) {
	for i, buf := range e.buffers {
		if buf.isConfig {
			e.activeBuffer = i
			buf.isReadOnly = isReadOnly // 💡 상태 덮어쓰기
			return
		}
	}
	path := getConfigPath()
	if path == "" {
		return
	}
	lines, encoding, totalChars, hash, endsWithNewline, err := loadFileLines(path, "")
	if err != nil {
		return
	}

	b := NewBuffer()
	b.filePath = path
	b.isConfig = true
	b.isReadOnly = isReadOnly
	e.fileWatcher.Add(path)
	b.lines = lines
	b.encoding = encoding
	b.vCache = make(map[int][]VisualLine)
	b.vLinesValid = false
	b.totalChars = totalChars
	b.savedTotalChars = totalChars
	b.currentHash = hash
	b.savedHash = hash
	b.endsWithNewline = endsWithNewline
	b.savedStrongHash = b.computeStrongHash()
	b.updateSavedFileInfo()

	if !e.initialBufferUsed && len(e.buffers) == 1 && e.buffers[0].filePath == "" && !e.buffers[0].isModified && !e.buffers[0].isConfig {
		e.buffers[0] = b
		e.activeBuffer = 0
		e.initialBufferUsed = true
	} else {
		e.buffers = append(e.buffers, b)
		e.activeBuffer = len(e.buffers) - 1
	}
}

// --- [ 6. 메인 이벤트 루프 ] ---

func (e *Editor) getActive() *Buffer {
	return e.buffers[e.activeBuffer]
}

func main() {
	// 📊 [Phase 0] 측정 인프라: env 가드로 평시 오버헤드 0
	// 사용법: JIGEDIT_PPROF=1 ./jigedit <파일>
	// 프로파일: go tool pprof http://localhost:6060/debug/pprof/profile?seconds=10
	if os.Getenv("JIGEDIT_PPROF") != "" {
		go func() { _ = http.ListenAndServe("localhost:6060", nil) }()
	}

	// 💡 CLI 스테이트 머신 파서
	var actions []StartupAction
	currentRO := false

	for _, arg := range os.Args[1:] {
		switch arg {
		case "-R", "--readonly":
			currentRO = true
		case "-e", "--edit":
			currentRO = false
		case "-c", "--config":
			actions = append(actions, StartupAction{Type: "config", ReadOnly: currentRO})
		case "-o", "--open":
			actions = append(actions, StartupAction{Type: "picker", ReadOnly: currentRO})
		case "-n", "--new":
			actions = append(actions, StartupAction{Type: "new", ReadOnly: currentRO})
		case "-v", "--version":
			fmt.Println("jigedit v1.3.2 - A Sane Editor For The Sane People")
			os.Exit(0)
		case "-h", "--help":
			fmt.Println("Usage: jigedit [FLAGS] [FILENAME]")
			fmt.Println("[FLAGS] (except -h,-v) can be stacked")
			fmt.Println("  -o, --open      Open file picker to select files")
			fmt.Println("  -c, --config    Open config.json")
			fmt.Println("  -n, --new       Open a new empty tab") // 💡 추가됨
			fmt.Println("  -R, --readonly  Open subsequent files in READ-ONLY mode")
			fmt.Println("  -e, --edit      Open subsequent files in EDIT mode (default)")
			fmt.Println("  -v, --version   Print version")
			fmt.Println("  -h, --help      Print help")
			fmt.Println("\nExample: jigedit -c -o -R file1.txt -e file2.txt -o -R folder/file3.txt")
			os.Exit(0)
		default:
			// 짧은 플래그 결합 지원 (-Rc, -Ro 등)
			if strings.HasPrefix(arg, "-") && len(arg) > 1 && !strings.HasPrefix(arg, "--") {
				for _, ch := range arg[1:] {
					if ch == 'R' {
						currentRO = true
					}
					if ch == 'e' {
						currentRO = false
					}
					if ch == 'c' {
						actions = append(actions, StartupAction{Type: "config", ReadOnly: currentRO})
					}
					if ch == 'o' {
						actions = append(actions, StartupAction{Type: "picker", ReadOnly: currentRO})
					}
				}
			} else {
				actions = append(actions, StartupAction{Type: "file", Path: arg, ReadOnly: currentRO})
			}
		}
	}

	s, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("%v", err)
	}
	if err := s.Init(); err != nil {
		log.Fatalf("%v", err)
	}
	defer s.Fini()

	s.EnableMouse(tcell.MouseMotionEvents)
	globalScreenHandle = &s
	editor := NewEditor()
	defer editor.fileWatcher.Close()

	// 💡 파싱된 큐(Queue) 순차 실행 엔진
	if len(actions) > 0 {
		w, _ := s.Size()
		// 파일 픽커가 뜨기 전에 에디터 껍데기를 먼저 그려줍니다.
		editor.getActive().generateVisualLines(w, editor.cfg)
		editor.draw(s) // 🟢 cachedVLines 인자 제거

		for _, action := range actions {
			if action.Type == "file" {
				editor.openOrFocusFile(action.Path, action.ReadOnly)
			} else if action.Type == "config" {
				editor.focusOrOpenConfig(action.ReadOnly)
			} else if action.Type == "picker" {
				s.Suspend()
				filePath, err := zenity.SelectFile(zenity.Title("파일 열기"))
				s.Resume()
				s.Sync()
				if err == nil && filePath != "" {
					editor.openOrFocusFile(filePath, action.ReadOnly)
				}
			} else if action.Type == "new" { // 💡 여기서부터 추가됨
				b := NewBuffer()
				b.isReadOnly = action.ReadOnly
				if !editor.initialBufferUsed && len(editor.buffers) == 1 && editor.buffers[0].filePath == "" && !editor.buffers[0].isModified && !editor.buffers[0].isConfig {
					editor.buffers[0] = b
					editor.activeBuffer = 0
					editor.initialBufferUsed = true
				} else {
					editor.buffers = append(editor.buffers, b)
					editor.activeBuffer = len(editor.buffers) - 1
				}
			}
		}
		editor.needsFullRefresh = true
	}

	needsLayout := true
	snapToCursor := true

	var lastClickTime time.Time
	var lastClickX, lastClickY int
	var clickCount int
	var lastMouseButtons tcell.ButtonMask

	for {
		currentScreen := *globalScreenHandle
		w, h := currentScreen.Size()
		b := editor.getActive()

		// 💡 커서나 스크롤이 가상 윈도우 범위를 벗어나면 즉시 렌더링 강제 트리거
		if needsLayout && !currentScreen.HasPendingEvent() {
			b.generateVisualLines(w, editor.cfg) // 🟢 cachedVLines 대입 제거
			needsLayout = false
		}

		if snapToCursor && !editor.paletteActive && !editor.ctxMenuActive && !editor.encodeMenuActive && !currentScreen.HasPendingEvent() {
			b.scrollToCursorV(editor.cfg, h-editor.tabHeight-1)
			textMaxWidth := w - b.getLineNumWidth(editor.cfg)

			// 💡 [버그 수정] 줄바꿈 상태와 관계없이 무조건 스크롤 추적 엔진을 사용합니다!
			// 기존에 있던 else { b.hOffset = 0 } 코드를 삭제했습니다.
			b.scrollToCursorH(textMaxWidth, editor.cfg)

			snapToCursor = false
		}

		if !currentScreen.HasPendingEvent() {
			editor.draw(currentScreen) // 🟢 cachedVLines 인자 제거
		}

		ev := currentScreen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventResize:
			currentScreen.Sync()
			needsLayout = true
			snapToCursor = true
			editor.needsFullRefresh = true
		case *tcell.EventInterrupt:
			filePath, ok := ev.Data().(string)
			if ok {
				// 💡 [안전장치 아키텍처]: 가변 슬라이스의 루프 인덱스 직접 참조를 폐기하고 안전하게 스냅샷 참조
				var targetBufs []*Buffer
				for _, buf := range editor.buffers {
					if buf != nil && buf.filePath == filePath {
						targetBufs = append(targetBufs, buf)
					}
				}

				for _, buf := range targetBufs {
					// 현재 활성화된 에디터 버퍼 슬라이스 내에 여전히 존재하는지 재차 교차 검증
					exists := false
					targetIdx := -1
					for idx, currentBuf := range editor.buffers {
						if currentBuf == buf {
							exists = true
							targetIdx = idx
							break
						}
					}
					if !exists || targetIdx == -1 {
						continue
					}

					// 💡 우리가 방금 쓴 변경(저장)이면 디스크 재읽기 자체를 건너뜀
					if info, err := os.Stat(filePath); err == nil {
						if info.ModTime().Equal(buf.savedModTime) && info.Size() == buf.savedSize {
							continue
						}
					}

					// 💡 수정된 부분: 파일 감지기도 현재 탭의 인코딩을 존중합니다.
					lines, encoding, incomingChars, incomingHash, incomingEndsWithNL, err := loadFileLines(filePath, buf.encoding)
					if err == nil {
						if incomingHash != buf.savedHash || incomingChars != buf.savedTotalChars {
							if buf.isModified {
								alreadyQueued := false
								for _, q := range editor.externalChangeQueue {
									if q == targetIdx {
										alreadyQueued = true
										break
									}
								}
								if !alreadyQueued {
									editor.externalChangeQueue = append(editor.externalChangeQueue, targetIdx)
								}
								editor.promptMode = true
								editor.promptType = "external_change"
								editor.targetCloseBuffer = editor.externalChangeQueue[0]
								needsLayout = true
							} else {
								buf.reloadFromLines(lines, encoding, incomingChars, incomingHash, incomingEndsWithNL)
								if buf.isConfig {
									var newCfg Config
									if err := json.Unmarshal([]byte(buf.getContent()), &newCfg); err == nil {
										editor.applyConfig(newCfg)
									}
								}
								needsLayout = true
								snapToCursor = true
							}
						}
					}
				}
			}
		case *tcell.EventMouse:
			// ponytail: reset horizontal target memory on mouse interaction
			b := editor.getActive()
			b.cursor.TargetX = -1
			for i := range b.extraCursors {
				b.extraCursors[i].TargetX = -1
			}

			mx, my := ev.Position()
			mouseMoved := (editor.mouseX != mx || editor.mouseY != my)
			editor.mouseX = mx
			editor.mouseY = my
			buttons := ev.Buttons()
			isWheel := (buttons&tcell.WheelUp != 0) || (buttons&tcell.WheelDown != 0) || (buttons&tcell.WheelLeft != 0) || (buttons&tcell.WheelRight != 0)

			var isNewPress, isDrag bool
			if isWheel {
				isNewPress = false
				isDrag = (lastMouseButtons&tcell.Button1 != 0)
			} else {
				oldButtons := lastMouseButtons
				lastMouseButtons = buttons
				isNewPress = (buttons&tcell.Button1 != 0) && (oldButtons&tcell.Button1 == 0)
				isDrag = (buttons&tcell.Button1 != 0) && !isNewPress
			}

			if isNewPress {
				now := time.Now()
				if now.Sub(lastClickTime) < 400*time.Millisecond && mx == lastClickX && my == lastClickY {
					clickCount++
				} else {
					clickCount = 1
				}
				lastClickTime = now
				lastClickX, lastClickY = mx, my
			}

			if (buttons&tcell.Button3 != 0 || buttons&tcell.Button2 != 0) && !editor.paletteActive && !editor.keyMenuActive {
				// 💡 우클릭 시 우클릭한 위치로 커서(I-빔) 이동
				if my >= editor.tabHeight && my < h-1 {
					loc := b.screenToMemoryPosV(mx, my, editor.tabHeight, editor.cfg)
					// 💡 이미 선택된 영역 안을 우클릭한 것이라면 선택 영역을 유지하고,
					// 그 외의 지역을 우클릭한 것이라면 선택 영역을 해제하고 커서를 이동함
					if !b.isLocInSelection(loc) {
						b.clearSelection()
						b.cursor = loc
					}
				}

				editor.ctxMenuActive = true
				editor.ctxMenuX = mx
				editor.ctxMenuY = my
				editor.ctxMenuW = 0
				editor.ctxMenuH = 0
				editor.ctxMenuCursor = 0
				editor.paletteActive = false
				editor.encodeMenuActive = false
				needsLayout = true
				continue
			}

			if editor.ctxMenuActive {
				if mx >= editor.ctxMenuX && mx < editor.ctxMenuX+editor.ctxMenuW && my >= editor.ctxMenuY && my < editor.ctxMenuY+editor.ctxMenuH {
					_, screenH := currentScreen.Size()
					pageStep := screenH - 6
					if pageStep < 5 {
						pageStep = 5
					}
					visibleItems := editor.ctxMenuH - 2

					if isWheel {
						if buttons&tcell.WheelUp != 0 {
							editor.menuScrollOffset -= 3
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
						} else if buttons&tcell.WheelDown != 0 {
							editor.menuScrollOffset += 3
							maxOffset := len(editor.ctxMenuItems) - visibleItems
							if maxOffset < 0 {
								maxOffset = 0
							}
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
						}
						continue
					}

					clickIdx := my - editor.ctxMenuY - 1

					if isNewPress {
						if my == editor.ctxMenuY && mx >= editor.ctxMenuX+editor.ctxMenuW-4 && mx <= editor.ctxMenuX+editor.ctxMenuW-2 {
							editor.menuScrollOffset -= pageStep
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
							continue
						}
						if my == editor.ctxMenuY+editor.ctxMenuH-1 && mx >= editor.ctxMenuX+editor.ctxMenuW-4 && mx <= editor.ctxMenuX+editor.ctxMenuW-2 {
							editor.menuScrollOffset += pageStep
							maxOffset := len(editor.ctxMenuItems) - visibleItems
							if maxOffset < 0 {
								maxOffset = 0
							}
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
							continue
						}
					}

					if clickIdx >= 0 && clickIdx < visibleItems {
						targetItemIdx := editor.menuScrollOffset + clickIdx
						if targetItemIdx >= 0 && targetItemIdx < len(editor.ctxMenuItems) {
							if mouseMoved {
								editor.ctxMenuCursor = targetItemIdx
								needsLayout = true
							}
							if isNewPress {
								editor.ctxMenuCursor = targetItemIdx
								action := editor.ctxMenuItems[editor.ctxMenuCursor].Action
								editor.ctxMenuActive = false
								if action != nil {
									action(editor, currentScreen)
								}
								needsLayout = true
							}
						}
					}
				} else if isNewPress {
					editor.ctxMenuActive = false
					needsLayout = true
				}
				continue
			}

			if editor.encodeMenuActive {
				if mx >= editor.encodeMenuX && mx < editor.encodeMenuX+editor.encodeMenuW && my >= editor.encodeMenuY && my < editor.encodeMenuY+editor.encodeMenuH {
					_, screenH := currentScreen.Size()
					pageStep := screenH - 6
					if pageStep < 5 {
						pageStep = 5
					}
					visibleItems := editor.encodeMenuH - 2

					if isWheel {
						if buttons&tcell.WheelUp != 0 {
							editor.menuScrollOffset -= 3
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
						} else if buttons&tcell.WheelDown != 0 {
							editor.menuScrollOffset += 3
							maxOffset := len(editor.encodeMenuItems) - visibleItems
							if maxOffset < 0 {
								maxOffset = 0
							}
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
						}
						continue
					}

					clickIdx := my - editor.encodeMenuY - 1

					if isNewPress {
						if my == editor.encodeMenuY && mx >= editor.encodeMenuX+editor.encodeMenuW-4 && mx <= editor.encodeMenuX+editor.encodeMenuW-2 {
							editor.menuScrollOffset -= pageStep
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
							continue
						}
						if my == editor.encodeMenuY+editor.encodeMenuH-1 && mx >= editor.encodeMenuX+editor.encodeMenuW-4 && mx <= editor.encodeMenuX+editor.encodeMenuW-2 {
							editor.menuScrollOffset += pageStep
							maxOffset := len(editor.encodeMenuItems) - visibleItems
							if maxOffset < 0 {
								maxOffset = 0
							}
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
							continue
						}
					}

					if clickIdx >= 0 && clickIdx < visibleItems {
						targetItemIdx := editor.menuScrollOffset + clickIdx
						if targetItemIdx >= 0 && targetItemIdx < len(editor.encodeMenuItems) {
							if mouseMoved {
								editor.encodeMenuCursor = targetItemIdx
								needsLayout = true
							}
							if isNewPress {
								editor.encodeMenuCursor = targetItemIdx
								action := editor.encodeMenuItems[editor.encodeMenuCursor].Action
								editor.encodeMenuActive = false
								editor.encodeMenuState = 0
								if action != nil {
									action(editor, currentScreen)
								}
								needsLayout = true
							}
						}
					}
				} else if isNewPress {
					editor.encodeMenuActive = false
					editor.encodeMenuState = 0
					needsLayout = true
				}
				continue
			}

			if editor.keyMenuActive {
				inMenu := mx >= editor.keyMenuX && mx < editor.keyMenuX+editor.keyMenuW && my >= editor.keyMenuY && my < editor.keyMenuY+editor.keyMenuH

				// 💡 캡처 화면에서는 마우스로 할 수 있는 일이 "취소" 뿐이다.
				if editor.keyMenuState == 2 {
					if isNewPress && !inMenu {
						editor.backToKeybindList()
						needsLayout = true
					}
					continue
				}

				if inMenu {
					_, screenH := currentScreen.Size()
					pageStep := screenH - 6
					if pageStep < 5 {
						pageStep = 5
					}
					visibleItems := editor.keyMenuH - 2
					maxOffset := len(editor.keyMenuItems) - visibleItems
					if maxOffset < 0 {
						maxOffset = 0
					}

					if isWheel {
						if buttons&tcell.WheelUp != 0 {
							editor.menuScrollOffset -= 3
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
						} else if buttons&tcell.WheelDown != 0 {
							editor.menuScrollOffset += 3
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
						}
						continue
					}

					if isNewPress {
						// 💡 테두리에 그려진 ▲/▼ 화살표 클릭 = 페이지 스크롤
						if my == editor.keyMenuY && mx >= editor.keyMenuX+editor.keyMenuW-4 && mx <= editor.keyMenuX+editor.keyMenuW-2 {
							editor.menuScrollOffset -= pageStep
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
							continue
						}
						if my == editor.keyMenuY+editor.keyMenuH-1 && mx >= editor.keyMenuX+editor.keyMenuW-4 && mx <= editor.keyMenuX+editor.keyMenuW-2 {
							editor.menuScrollOffset += pageStep
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
							continue
						}
					}

					clickIdx := my - editor.keyMenuY - 1
					if clickIdx >= 0 && clickIdx < visibleItems {
						targetItemIdx := editor.menuScrollOffset + clickIdx
						if targetItemIdx >= 0 && targetItemIdx < len(editor.keyMenuItems) {
							if mouseMoved {
								editor.keyMenuCursor = targetItemIdx
								needsLayout = true
							}
							if isNewPress {
								editor.keyMenuCursor = targetItemIdx
								// 💡 팔레트/인코딩 메뉴와 달리 메뉴를 닫지 않는다 —
								// 항목 선택은 캡처 화면으로 넘어가는 것이지 종료가 아니다.
								if action := editor.keyMenuItems[targetItemIdx].Action; action != nil {
									action(editor, currentScreen)
								}
								needsLayout = true
							}
						}
					}
				} else if isNewPress {
					editor.closeKeybindMenu()
					needsLayout = true
				}
				continue
			}

			if !b.searchMode && !b.gotoMode && my == h-1 {
				if !b.isConfig && mx >= b.encodeBtnX1 && mx <= b.encodeBtnX2 && isNewPress {
					editor.showEncodeActionMenu(mx, h-6) // 💡 1단계 액션 메뉴 호출
					needsLayout = true
					continue
				}
			}

			if (b.searchMode || b.gotoMode) && (my == h-1 || (b.isInputSelect && isDrag)) {
				if my == h-1 && mx >= b.closeBtnStartX && mx <= b.closeBtnEndX && isNewPress {
					b.searchMode = false
					b.gotoMode = false
					b.isReplace = false
					b.isInputSelect = false
					needsLayout = true
					continue
				}

				if !b.gotoMode && my == h-1 && isNewPress {
					// 💡 토글 시 재검색 + 가장 가까운 곳으로 즉시 점프하는 함수
					doSearchJump := func() {
						b.findAllMatches(editor.cfg.OverlapSearch)
						if len(b.matches) > 0 {
							b.matchIdx = b.findInitialMatchIdx(b.cursor, false)
							b.jumpToMatch()
							snapToCursor = true // 화면 스크롤 즉시 추적
						}
						needsLayout = true
					}

					if mx >= b.chkRegexX1 && mx <= b.chkRegexX2 {
						b.searchRegex = !b.searchRegex
						doSearchJump()
						continue
					}
					if mx >= b.chkCaseX1 && mx <= b.chkCaseX2 {
						b.searchCase = !b.searchCase
						doSearchJump()
						continue
					}
					if mx >= b.chkWordX1 && mx <= b.chkWordX2 {
						b.searchWord = !b.searchWord
						doSearchJump()
						continue
					}
				}
				var prefix string
				var targetStr *[]rune
				if b.gotoMode {
					prefix = " [Go To] Line,Col: "
					targetStr = &b.gotoInput
				} else if b.isReplace {
					if b.replaceStep == 1 {
						prefix = " [Replace] Find: "
						targetStr = &b.searchQuery
					} else if b.replaceStep == 2 {
						prefix = " [Replace] Find: " + string(b.searchQuery) + "  ➔ Replace: "
						targetStr = &b.replaceQuery
					}
				} else {
					prefix = " [Find] Search: "
					targetStr = &b.searchQuery
				}

				if targetStr != nil {
					prefixW := runewidth.StringWidth(prefix)
					cbLen := 0
					if !b.gotoMode {
						cbLen = runewidth.StringWidth(" [ ] Regex  [ ] Case  [ ] Word ")
					}
					cbStartX := b.closeBtnStartX - cbLen

					suffix := ""
					if b.gotoMode {
						suffix = "  (Enter: 이동, Esc: 취소)"
					} else if !b.isReplace {
						matchCountStr := "0/0"
						if len(b.matches) > 0 {
							totStr := fmt.Sprintf("%d", len(b.matches))
							if b.searchCapped {
								totStr += "+"
							}
							matchCountStr = fmt.Sprintf("%d/%s", b.matchIdx+1, totStr)
						}
						suffix = "  [" + matchCountStr + "] (Enter/Down:다음, Up:이전)"
					}
					suffixW := runewidth.StringWidth(suffix)
					inputBoxEnd := cbStartX - suffixW
					if inputBoxEnd <= prefixW {
						inputBoxEnd = prefixW + 5
					}

					idx := 0
					if mx >= prefixW {
						leftArrow := 0
						if b.inputHOffset > 0 {
							leftArrow = 1
						}

						strW := 0
						visX := prefixW + leftArrow
						for i, r := range *targetStr {
							rw := runewidth.RuneWidth(r)
							if strW >= b.inputHOffset {
								if visX >= inputBoxEnd-1 {
									break
								}
								if mx < visX {
									idx = i
									break
								}
								if mx >= visX && mx < visX+rw {
									idx = i
									break
								}
								visX += rw
							}
							strW += rw
							idx = i + 1
						}
					}
					if isNewPress && my == h-1 {
						if clickCount == 1 {
							b.inputCX = idx
							b.isInputSelect = true
							b.inputSelStart = idx
							b.inputSelEnd = idx
						} else if clickCount == 2 {
							b.inputCX = idx
							if len(*targetStr) > 0 {
								c := idx
								if c >= len(*targetStr) {
									c = len(*targetStr) - 1
								}
								if c >= 0 {
									r := (*targetStr)[c]
									isSp := unicode.IsSpace(r)
									isAlpha := isWordChar(r)

									left := c
									for left > 0 {
										pr := (*targetStr)[left-1]
										if isSp {
											if !unicode.IsSpace(pr) {
												break
											}
										} else {
											if unicode.IsSpace(pr) || isWordChar(pr) != isAlpha {
												break
											}
										}
										left--
									}

									right := c
									for right < len(*targetStr) {
										cr := (*targetStr)[right]
										if isSp {
											if !unicode.IsSpace(cr) {
												break
											}
										} else {
											if unicode.IsSpace(cr) || isWordChar(cr) != isAlpha {
												break
											}
										}
										right++
									}

									b.isInputSelect = true
									b.inputSelStart = left
									b.inputSelEnd = right
									b.inputCX = right
								}
							}
						} else if clickCount >= 3 {
							b.isInputSelect = true
							b.inputSelStart = 0
							b.inputSelEnd = len(*targetStr)
							b.inputCX = len(*targetStr)
							clickCount = 0
						}
					} else if isDrag && b.isInputSelect {
						b.inputCX = idx
						b.inputSelEnd = idx
						b.isInputSelect = true
					}
					needsLayout = true
				}
				if my == h-1 || b.isInputSelect {
					continue
				}
			}

			if editor.paletteActive {
				if mx >= editor.paletteX && mx < editor.paletteX+editor.paletteW && my >= editor.paletteY && my < editor.paletteY+editor.paletteH {
					_, screenH := currentScreen.Size()
					pageStep := screenH - 6
					if pageStep < 5 {
						pageStep = 5
					}
					visibleItems := editor.paletteH - 2

					if isWheel {
						if buttons&tcell.WheelUp != 0 {
							editor.menuScrollOffset -= 3
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
						} else if buttons&tcell.WheelDown != 0 {
							editor.menuScrollOffset += 3
							maxOffset := len(editor.paletteItems) - visibleItems
							if maxOffset < 0 {
								maxOffset = 0
							}
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
						}
						continue
					}

					clickIdx := my - editor.paletteY - 1

					if isNewPress {
						if my == editor.paletteY && mx >= editor.paletteX+editor.paletteW-4 && mx <= editor.paletteX+editor.paletteW-2 {
							editor.menuScrollOffset -= pageStep
							if editor.menuScrollOffset < 0 {
								editor.menuScrollOffset = 0
							}
							needsLayout = true
							continue
						}
						if my == editor.paletteY+editor.paletteH-1 && mx >= editor.paletteX+editor.paletteW-4 && mx <= editor.paletteX+editor.paletteW-2 {
							editor.menuScrollOffset += pageStep
							maxOffset := len(editor.paletteItems) - visibleItems
							if maxOffset < 0 {
								maxOffset = 0
							}
							if editor.menuScrollOffset > maxOffset {
								editor.menuScrollOffset = maxOffset
							}
							needsLayout = true
							continue
						}
					}

					if clickIdx >= 0 && clickIdx < visibleItems {
						targetItemIdx := editor.menuScrollOffset + clickIdx
						if targetItemIdx >= 0 && targetItemIdx < len(editor.paletteItems) {
							if mouseMoved {
								editor.paletteCursor = targetItemIdx
								needsLayout = true
							}
							if isNewPress {
								editor.paletteCursor = targetItemIdx
								action := editor.paletteItems[editor.paletteCursor].Action
								editor.paletteActive = false
								if action != nil {
									action(editor, currentScreen)
								}
								needsLayout = true
							}
						}
					}
				} else if isNewPress {
					editor.paletteActive = false
					needsLayout = true
				}
				continue
			}

			if my < editor.tabHeight {
				if isNewPress {
					for _, tb := range editor.tabBounds {
						if my == tb.Y && mx >= tb.StartX && mx < tb.EndX {
							// 💡 이미 보고 있는 탭을 또 누르면 아예 무시해서 화면 깜빡임 방지
							if editor.activeBuffer != tb.Idx {
								editor.activeBuffer = tb.Idx
								editor.needsFullRefresh = true
								needsLayout = true
							}
							break
						}
					}
					continue
				}
				if buttons&tcell.WheelUp != 0 {
					editor.activeBuffer = (editor.activeBuffer - 1 + len(editor.buffers)) % len(editor.buffers)
					editor.needsFullRefresh = true
					needsLayout = true
					continue
				}
				if buttons&tcell.WheelDown != 0 {
					editor.activeBuffer = (editor.activeBuffer + 1) % len(editor.buffers)
					editor.needsFullRefresh = true
					needsLayout = true
					continue
				}
			}
			outOfBounds := my < editor.tabHeight || my >= h-1
			if buttons&tcell.WheelUp != 0 {
				if !outOfBounds {
					b.vOffsetSub--
					if b.vOffsetSub < 0 {
						if b.vOffsetL > 0 {
							b.vOffsetL--
							b.ensureVCache(b.vOffsetL, editor.cfg)
							b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
						} else {
							b.vOffsetSub = 0
						}
					}
					needsLayout = true
				}
				continue
			}
			if buttons&tcell.WheelDown != 0 {
				if !outOfBounds {
					b.vOffsetSub++
					b.ensureVCache(b.vOffsetL, editor.cfg)
					if b.vOffsetSub >= len(b.vCache[b.vOffsetL]) {
						if b.vOffsetL+1 < len(b.lines) {
							b.vOffsetL++
							b.vOffsetSub = 0
						} else {
							b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
						}
					}
					needsLayout = true
				}
				continue
			}
			if buttons&tcell.Button1 != 0 {
				isIndicatorEvent := false
				if !outOfBounds {
					relativeY := my - editor.tabHeight
					if relativeY >= 0 {
						currL := b.vOffsetL
						currSub := b.vOffsetSub
						for i := 0; i < relativeY; i++ {
							b.ensureVCache(currL, editor.cfg)
							currSub++
							if currSub >= len(b.vCache[currL]) {
								currSub = 0
								currL++
							}
							if currL >= len(b.lines) {
								currL = len(b.lines) - 1
								b.ensureVCache(currL, editor.cfg)
								currSub = len(b.vCache[currL]) - 1
								break
							}
						}

						if currL < len(b.lines) {
							b.ensureVCache(currL, editor.cfg)
							vl := b.vCache[currL][currSub]
							lineNumWidth := b.getLineNumWidth(editor.cfg)
							textMaxWidth := w - lineNumWidth
							if textMaxWidth <= 0 {
								textMaxWidth = 1
							}

							vlWidth := vl.width

							leftIndX := lineNumWidth - 1
							if lineNumWidth <= 0 {
								leftIndX = 0
							}
							if b.hOffset > 0 && vlWidth > 0 && mx <= leftIndX+1 {
								isIndicatorEvent = true
								if isNewPress || isDrag {
									b.hOffset -= 5
									if b.hOffset < 0 {
										b.hOffset = 0
									}
								}
							}

							if !isIndicatorEvent {
								if vlWidth > b.hOffset+textMaxWidth && mx >= w-2 {
									isIndicatorEvent = true
									if isNewPress || isDrag {
										b.hOffset += 5
										if b.hOffset+textMaxWidth > vlWidth {
											b.hOffset = vlWidth - textMaxWidth + 1
										}
									}
								}
							}
						}
					}
				}

				if isIndicatorEvent {
					needsLayout = true
					snapToCursor = false
					continue
				}

				loc := b.screenToMemoryPosV(mx, my, editor.tabHeight, editor.cfg)
				if isNewPress && outOfBounds {
					continue
				}

				// ponytail: Ctrl+Click toggles a cursor -- adds one, unless loc
				// already has a caret on it, in which case that caret is removed.
				// Like micro, holding Ctrl blocks the drag entirely: only the
				// initial press does anything, and any drag/move while Ctrl is
				// still held is ignored instead of falling through into normal
				// text-selection dragging (which produced garbled selections).
				if (ev.Modifiers() & tcell.ModCtrl) != 0 {
					if isNewPress {
						if !b.removeCursorAt(loc) {
							b.extraCursors = append(b.extraCursors, loc)
							b.cleanExtraCursors()
						}
						editor.needsFullRefresh = true
						needsLayout = true
					}
					continue
				}

				// ponytail: any plain click clears extra cursors
				if isNewPress && len(b.extraCursors) > 0 {
					b.clearExtraCursors()
					editor.needsFullRefresh = true
				}

				if isNewPress {
					b.isInputSelect = false // 💡 에디터를 직접 누르면 status/search input 선택 영역 해제
					if clickCount == 1 {
						if !b.isSelecting {
							b.isSelecting = true
							b.selection.Start = loc
						}
						b.selection.End = loc
						b.cursor = loc
					} else if clickCount == 2 {
						b.cursor = loc
						b.selectWordAtCursor()
					} else if clickCount >= 3 {
						b.cursor = loc
						b.selectLineAtCursor()
						clickCount = 0
					}
				} else {

					// 💡 마우스가 완벽히 멈춰있을 때는 더블/트리플 클릭 선택 영역을 취소하지 않음!
					if mx != lastClickX || my != lastClickY {
						b.selection.End = loc
						b.cursor = loc
					}
					// 🟢 O(1) 가상라인 역방향 이동
					if my < editor.tabHeight && (b.vOffsetL > 0 || b.vOffsetSub > 0) {
						b.vOffsetSub--
						if b.vOffsetSub < 0 {
							b.vOffsetL--
							b.ensureVCache(b.vOffsetL, editor.cfg)
							b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
						}
						// 💡 [추가] 스크롤되어 새로 나타난 맨 윗줄 좌표를 즉시 다시 계산해서 주입!
						loc = b.screenToMemoryPosV(mx, my, editor.tabHeight, editor.cfg)
						b.selection.End = loc
						b.cursor = loc
						// 🟢 O(1) 가상라인 순방향 이동
					} else if my >= h-1 {
						b.vOffsetSub++
						b.ensureVCache(b.vOffsetL, editor.cfg)
						if b.vOffsetSub >= len(b.vCache[b.vOffsetL]) {
							if b.vOffsetL+1 < len(b.lines) {
								b.vOffsetL++
								b.vOffsetSub = 0
							} else {
								b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
							}
						}
						// 💡 [추가] 아래쪽도 스크롤 직후 새 줄 좌표를 즉시 동기화!
						loc = b.screenToMemoryPosV(mx, my, editor.tabHeight, editor.cfg)
						b.selection.End = loc
						b.cursor = loc
					}
				}
				snapToCursor = true
			} else {
				if b.isSelecting {
					b.isSelecting = false
					if b.selection.Start == b.selection.End {
						b.clearSelection()
					}
				}
			}
		case *tcell.EventKey:
			// ponytail: reset horizontal target memory on any key EXCEPT vertical movement
			b := editor.getActive()
			k := ev.Key()
			if k != tcell.KeyUp && k != tcell.KeyDown && k != tcell.KeyPgUp && k != tcell.KeyPgDn {
				b.cursor.TargetX = -1
				for i := range b.extraCursors {
					b.extraCursors[i].TargetX = -1
				}
			}

			isCtrl := (ev.Modifiers() & tcell.ModCtrl) != 0
			isShift := (ev.Modifiers() & tcell.ModShift) != 0
			isAlt := (ev.Modifiers() & tcell.ModAlt) != 0
			snapToCursor = true

			if ev.Key() != tcell.KeyEnd {
				b.stickToWrapEnd = false
			}

			// 💡 PreModal 액션(탭 전환)은 팔레트/검색창/프롬프트가 열려 있어도 동작한다.
			// 여기서 처리하지 않으면 검색 중 Alt+. 로 탭을 넘길 수 없게 된다.
			evChord := chordFromEvent(ev)
			if act := editor.bindings[evChord]; act != nil && act.PreModal {
				act.Fn(editor, currentScreen)
				needsLayout = true
				if act.NoSnap {
					snapToCursor = false
				}
				continue
			}

			if editor.promptMode {
				snapToCursor = false // 💡 프롬프트 입력 및 취소 시 화면 점프 방지
				// 💡 Alert 모드일 때는 y/n이 아니라 Enter나 Esc로 단순히 닫음
				if editor.promptType == "alert" {
					if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyEnter {
						editor.promptMode = false
						needsLayout = true
					}
					continue
				}

				if ev.Key() == tcell.KeyEscape || ev.Rune() == 'n' || ev.Rune() == 'N' {
					editor.promptMode = false
					if editor.promptType == "external_change" && len(editor.externalChangeQueue) > 0 {
						editor.externalChangeQueue = editor.externalChangeQueue[1:]
						if len(editor.externalChangeQueue) > 0 {
							editor.promptMode = true
							editor.targetCloseBuffer = editor.externalChangeQueue[0]
						}
					}
					needsLayout = true
				} else if ev.Key() == tcell.KeyEnter || ev.Rune() == 'y' || ev.Rune() == 'Y' {
					editor.promptMode = false

					// 💡 큐를 진행하기 전에 현재 타겟 탭을 캡처합니다.
					target := editor.targetCloseBuffer
					isValidTarget := target >= 0 && target < len(editor.buffers)

					// advance queue
					if editor.promptType == "external_change" && len(editor.externalChangeQueue) > 0 {
						editor.externalChangeQueue = editor.externalChangeQueue[1:]
						if len(editor.externalChangeQueue) > 0 {
							editor.promptMode = true
							editor.targetCloseBuffer = editor.externalChangeQueue[0] // next prompt
						}
					}

					if editor.promptType == "quit" {
						shuttingDown.Store(true)
						currentScreen.Fini()
						os.Exit(0)
					} else if editor.promptType == "close" {
						if !isValidTarget {
							continue
						}
						if len(editor.buffers) <= 1 {
							shuttingDown.Store(true)
							currentScreen.Fini()
							os.Exit(0)
						}
						editor.closeBuffer(target)
						editor.needsFullRefresh = true
						needsLayout = true
					} else if editor.promptType == "reset_config" {
						// 💡 단축키는 건드리지 않는다 — 그건 "모든 단축키 기본값으로
						// 되돌리기" 전용 액션의 몫이다. 합쳐 놓으면 화면 설정 하나
						// 초기화하려다 애써 바꾼 단축키까지 통째로 날아간다.
						defaultCfg := DefaultConfig()
						defaultCfg.Keybindings = editor.cfg.Keybindings
						_ = SaveConfig(defaultCfg)
						editor.applyConfig(defaultCfg)
						for _, buf := range editor.buffers {
							if buf.isConfig {
								buf.isModified = false
								buf.reloadFromDisk()
							}
						}
						editor.needsFullRefresh = true
						needsLayout = true
					} else if editor.promptType == "reset_keybinds" {
						// 💡 단축키만 전부 기본값으로. 나머지 설정은 건드리지 않는다.
						cfg := editor.cfg
						cfg.Keybindings = nil
						editor.applyConfig(cfg)
						_ = SaveConfig(editor.cfg)
						editor.syncConfigBuffers()
						if editor.keyMenuActive {
							editor.showKeybindMenu()
						}
						needsLayout = true
					} else if editor.promptType == "reopen" {
						editor.getActive().reopenWithEncoding(editor.targetEncoding)
						editor.needsFullRefresh = true
						needsLayout = true
					} else if editor.promptType == "close_config" {
						if !isValidTarget {
							continue
						}
						editor.closeBuffer(target)
						editor.activeBuffer = 0
						editor.needsFullRefresh = true
						needsLayout = true
					} else if editor.promptType == "external_change" {
						if !isValidTarget {
							continue
						}
						bufToReload := editor.buffers[target]
						bufToReload.isModified = false
						if bufToReload.reloadFromDisk() {
							if bufToReload.isConfig {
								var newCfg Config
								if err := json.Unmarshal([]byte(bufToReload.getContent()), &newCfg); err == nil {
									editor.applyConfig(newCfg)
								}
							}
						}
						editor.needsFullRefresh = true
						needsLayout = true
						snapToCursor = true
					}
				}
				continue
			}

			if editor.paletteActive {
				snapToCursor = false // 💡 팔레트 조작 및 종료 시 화면 점프 방지
				_, screenH := currentScreen.Size()
				pageStep := screenH - 6
				if pageStep < 5 {
					pageStep = 5
				}

				switch ev.Key() {
				case tcell.KeyEscape:
					editor.paletteActive = false
				case tcell.KeyUp:
					editor.paletteCursor--
					if editor.paletteCursor < 0 {
						editor.paletteCursor = len(editor.paletteItems) - 1
					}
				case tcell.KeyDown:
					editor.paletteCursor++
					if editor.paletteCursor >= len(editor.paletteItems) {
						editor.paletteCursor = 0
					}
				case tcell.KeyPgUp:
					editor.paletteCursor -= pageStep
					if editor.paletteCursor < 0 {
						editor.paletteCursor = 0
					}
				case tcell.KeyPgDn:
					editor.paletteCursor += pageStep
					if editor.paletteCursor >= len(editor.paletteItems) {
						editor.paletteCursor = len(editor.paletteItems) - 1
					}
				case tcell.KeyEnter:
					action := editor.paletteItems[editor.paletteCursor].Action
					editor.paletteActive = false
					if action != nil {
						action(editor, currentScreen)
					}
				case tcell.KeyRune:
					if idx := getMenuJumpIdx(editor.paletteItems, ev.Rune(), editor.paletteCursor); idx != -1 {
						editor.paletteCursor = idx
					}
				}

				// 💡 자동 스크롤 추적 보정
				visibleItems := editor.paletteH - 2
				if visibleItems > 0 {
					if editor.paletteCursor < editor.menuScrollOffset {
						editor.menuScrollOffset = editor.paletteCursor
					} else if editor.paletteCursor >= editor.menuScrollOffset+visibleItems {
						editor.menuScrollOffset = editor.paletteCursor - visibleItems + 1
					}
				}
				needsLayout = true
				continue
			}

			if editor.ctxMenuActive {
				snapToCursor = false // 💡 컨텍스트 메뉴 조작 및 종료 시 화면 점프 방지
				_, screenH := currentScreen.Size()
				pageStep := screenH - 6
				if pageStep < 5 {
					pageStep = 5
				}

				switch ev.Key() {
				case tcell.KeyEscape:
					editor.ctxMenuActive = false
				case tcell.KeyUp:
					editor.ctxMenuCursor--
					if editor.ctxMenuCursor < 0 {
						editor.ctxMenuCursor = len(editor.ctxMenuItems) - 1
					}
				case tcell.KeyDown:
					editor.ctxMenuCursor++
					if editor.ctxMenuCursor >= len(editor.ctxMenuItems) {
						editor.ctxMenuCursor = 0
					}
				case tcell.KeyPgUp:
					editor.ctxMenuCursor -= pageStep
					if editor.ctxMenuCursor < 0 {
						editor.ctxMenuCursor = 0
					}
				case tcell.KeyPgDn:
					editor.ctxMenuCursor += pageStep
					if editor.ctxMenuCursor >= len(editor.ctxMenuItems) {
						editor.ctxMenuCursor = len(editor.ctxMenuItems) - 1
					}
				case tcell.KeyEnter:
					action := editor.ctxMenuItems[editor.ctxMenuCursor].Action
					editor.ctxMenuActive = false
					if action != nil {
						action(editor, currentScreen)
					}
				case tcell.KeyRune:
					if idx := getMenuJumpIdx(editor.ctxMenuItems, ev.Rune(), editor.ctxMenuCursor); idx != -1 {
						editor.ctxMenuCursor = idx
					}
				}

				// 💡 자동 스크롤 추적 보정
				visibleItems := editor.ctxMenuH - 2
				if visibleItems > 0 {
					if editor.ctxMenuCursor < editor.menuScrollOffset {
						editor.menuScrollOffset = editor.ctxMenuCursor
					} else if editor.ctxMenuCursor >= editor.menuScrollOffset+visibleItems {
						editor.menuScrollOffset = editor.ctxMenuCursor - visibleItems + 1
					}
				}
				needsLayout = true
				continue
			}

			if editor.encodeMenuActive {
				snapToCursor = false // 💡 인코딩 메뉴 조작 및 종료 시 화면 점프 방지
				_, screenH := currentScreen.Size()
				pageStep := screenH - 6
				if pageStep < 5 {
					pageStep = 5
				}

				switch ev.Key() {
				case tcell.KeyEscape:
					editor.encodeMenuActive = false
					editor.encodeMenuState = 0
				case tcell.KeyUp:
					editor.encodeMenuCursor--
					if editor.encodeMenuCursor < 0 {
						editor.encodeMenuCursor = len(editor.encodeMenuItems) - 1
					}
				case tcell.KeyDown:
					editor.encodeMenuCursor++
					if editor.encodeMenuCursor >= len(editor.encodeMenuItems) {
						editor.encodeMenuCursor = 0
					}
				case tcell.KeyPgUp:
					editor.encodeMenuCursor -= pageStep
					if editor.encodeMenuCursor < 0 {
						editor.encodeMenuCursor = 0
					}
				case tcell.KeyPgDn:
					editor.encodeMenuCursor += pageStep
					if editor.encodeMenuCursor >= len(editor.encodeMenuItems) {
						editor.encodeMenuCursor = len(editor.encodeMenuItems) - 1
					}
				case tcell.KeyEnter:
					action := editor.encodeMenuItems[editor.encodeMenuCursor].Action
					editor.encodeMenuActive = false
					editor.encodeMenuState = 0
					if action != nil {
						action(editor, currentScreen)
					}
				case tcell.KeyRune:
					if idx := getMenuJumpIdx(editor.encodeMenuItems, ev.Rune(), editor.encodeMenuCursor); idx != -1 {
						editor.encodeMenuCursor = idx
					}
				}

				// 💡 자동 스크롤 추적 보정
				visibleItems := editor.encodeMenuH - 2
				if visibleItems > 0 {
					if editor.encodeMenuCursor < editor.menuScrollOffset {
						editor.menuScrollOffset = editor.encodeMenuCursor
					} else if editor.encodeMenuCursor >= editor.menuScrollOffset+visibleItems {
						editor.menuScrollOffset = editor.encodeMenuCursor - visibleItems + 1
					}
				}
				needsLayout = true
				continue
			}

			if editor.keyMenuActive {
				snapToCursor = false // 💡 메뉴 조작 및 종료 시 화면 점프 방지

				// 💡 state 2: 새 키 캡처. 여기서는 모든 키 입력을 삼킨다.
				if editor.keyMenuState == 2 {
					switch ev.Key() {
					case tcell.KeyEscape:
						editor.backToKeybindList()
					case tcell.KeyDelete:
						if a, ok := actionByID[editor.keyMenuTargetID]; ok {
							editor.commitKeybind(a.ID, a.Def) // 기본값 복원
						}
					case tcell.KeyBackspace, tcell.KeyBackspace2:
						editor.commitKeybind(editor.keyMenuTargetID, KeyChord{}) // 해제
					default:
						editor.commitKeybind(editor.keyMenuTargetID, chordFromEvent(ev))
					}
					needsLayout = true
					continue
				}

				_, screenH := currentScreen.Size()
				pageStep := screenH - 6
				if pageStep < 5 {
					pageStep = 5
				}

				switch ev.Key() {
				case tcell.KeyEscape:
					editor.closeKeybindMenu()
				case tcell.KeyUp:
					editor.keyMenuCursor--
					if editor.keyMenuCursor < 0 {
						editor.keyMenuCursor = len(editor.keyMenuItems) - 1
					}
				case tcell.KeyDown:
					editor.keyMenuCursor++
					if editor.keyMenuCursor >= len(editor.keyMenuItems) {
						editor.keyMenuCursor = 0
					}
				case tcell.KeyPgUp:
					editor.keyMenuCursor -= pageStep
					if editor.keyMenuCursor < 0 {
						editor.keyMenuCursor = 0
					}
				case tcell.KeyPgDn:
					editor.keyMenuCursor += pageStep
					if editor.keyMenuCursor >= len(editor.keyMenuItems) {
						editor.keyMenuCursor = len(editor.keyMenuItems) - 1
					}
				case tcell.KeyEnter:
					if editor.keyMenuCursor >= 0 && editor.keyMenuCursor < len(editor.keyMenuItems) {
						if action := editor.keyMenuItems[editor.keyMenuCursor].Action; action != nil {
							action(editor, currentScreen)
						}
					}
				case tcell.KeyRune:
					if idx := getMenuJumpIdx(editor.keyMenuItems, ev.Rune(), editor.keyMenuCursor); idx != -1 {
						editor.keyMenuCursor = idx
					}
				}

				// 💡 자동 스크롤 추적 보정 (캡처 화면으로 넘어갔으면 건드리지 않는다)
				if editor.keyMenuState == 1 {
					editor.followKeybindCursor()
				}
				editor.needsFullRefresh = true
				needsLayout = true
				continue
			}

			if b.searchMode || b.gotoMode {
				snapToCursor = false // 💡 검색/이동 모드 입력 및 취소 시 화면 점프 방지 (단, 매치 이동 시에는 명시적으로 true 설정됨)
				if ev.Key() == tcell.KeyEscape {
					b.searchMode = false
					b.isReplace = false
					b.replaceStep = 0
					b.gotoMode = false
					b.clearSelection()
					b.isInputSelect = false
					needsLayout = true
					continue
				}

				var targetStr *[]rune
				if b.gotoMode {
					targetStr = &b.gotoInput
				} else if b.isReplace && b.replaceStep == 1 {
					targetStr = &b.searchQuery
				} else if b.isReplace && b.replaceStep == 2 {
					targetStr = &b.replaceQuery
				} else if !b.isReplace {
					targetStr = &b.searchQuery
				}

				if targetStr != nil {
					hasSel := b.isInputSelect && b.inputSelStart != b.inputSelEnd
					selStart, selEnd := b.inputSelStart, b.inputSelEnd
					if selStart > selEnd {
						selStart, selEnd = selEnd, selStart
					}

					deleteInputSel := func() {
						if hasSel {
							*targetStr = append((*targetStr)[:selStart], (*targetStr)[selEnd:]...)
							b.inputCX = selStart
							b.isInputSelect = false
							b.inputSelStart = 0
							b.inputSelEnd = 0
						}
					}

					if ev.Key() == tcell.KeyCtrlA {
						b.isInputSelect = true
						b.inputSelStart = 0
						b.inputSelEnd = len(*targetStr)
						b.inputCX = len(*targetStr)
						needsLayout = true
						continue
					}
					if ev.Key() == tcell.KeyCtrlC {
						if hasSel {
							clipboard.WriteAll(string((*targetStr)[selStart:selEnd]))
						} else {
							text := b.getSelectedText()
							if text != "" {
								_ = clipboard.WriteAll(text)
							}
						}
						continue
					}
					if ev.Key() == tcell.KeyCtrlX && hasSel {
						clipboard.WriteAll(string((*targetStr)[selStart:selEnd]))
						deleteInputSel()
						if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
							b.matches = nil
							b.matchIdx = -1
						}
						needsLayout = true
						continue
					}
					if ev.Key() == tcell.KeyCtrlV {
						text, err := clipboard.ReadAll()
						if err == nil && text != "" {
							deleteInputSel()
							text = strings.ReplaceAll(text, "\r\n", " ")
							text = strings.ReplaceAll(text, "\n", " ")
							text = strings.ReplaceAll(text, "\r", "")
							runes := []rune(text)
							*targetStr = append((*targetStr)[:b.inputCX], append(runes, (*targetStr)[b.inputCX:]...)...)
							b.inputCX += len(runes)
							if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
								b.matches = nil
								b.matchIdx = -1
							}
							needsLayout = true
						}
						continue
					}

					if ev.Key() == tcell.KeyLeft {
						if !isShift && hasSel {
							b.isInputSelect = false
							b.inputCX = selStart
						} else {
							if b.inputCX > 0 {
								b.inputCX--
							}
							if isShift {
								if !b.isInputSelect {
									b.isInputSelect = true
									b.inputSelStart = b.inputCX + 1
								}
								b.inputSelEnd = b.inputCX
							} else {
								b.isInputSelect = false
							}
						}
						needsLayout = true
						continue
					}
					if ev.Key() == tcell.KeyRight {
						if !isShift && hasSel {
							b.isInputSelect = false
							b.inputCX = selEnd
						} else {
							if b.inputCX < len(*targetStr) {
								b.inputCX++
							}
							if isShift {
								if !b.isInputSelect {
									b.isInputSelect = true
									b.inputSelStart = b.inputCX - 1
								}
								b.inputSelEnd = b.inputCX
							} else {
								b.isInputSelect = false
							}
						}
						needsLayout = true
						continue
					}
					if ev.Key() == tcell.KeyHome {
						if !isShift && hasSel {
							b.isInputSelect = false
						}
						if isShift {
							if !b.isInputSelect {
								b.isInputSelect = true
								b.inputSelStart = b.inputCX
							}
							b.inputCX = 0
							b.inputSelEnd = 0
						} else {
							b.inputCX = 0
							b.isInputSelect = false
						}
						needsLayout = true
						continue
					}
					if ev.Key() == tcell.KeyEnd {
						if !isShift && hasSel {
							b.isInputSelect = false
						}
						if isShift {
							if !b.isInputSelect {
								b.isInputSelect = true
								b.inputSelStart = b.inputCX
							}
							b.inputCX = len(*targetStr)
							b.inputSelEnd = b.inputCX
						} else {
							b.inputCX = len(*targetStr)
							b.isInputSelect = false
						}
						needsLayout = true
						continue
					}

					if ev.Key() == tcell.KeyBackspace || ev.Key() == tcell.KeyBackspace2 {
						if hasSel {
							deleteInputSel()
							if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
								b.matches = nil
								b.matchIdx = -1
							}
							needsLayout = true
						} else if b.inputCX > 0 {
							*targetStr = append((*targetStr)[:b.inputCX-1], (*targetStr)[b.inputCX:]...)
							b.inputCX--
							if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
								b.matches = nil
								b.matchIdx = -1
							}
							needsLayout = true
						} else if b.inputCX == 0 && b.isReplace && b.replaceStep == 2 {
							b.replaceStep = 1
							b.inputCX = len(b.searchQuery)
							b.isInputSelect = false
							needsLayout = true
						}
						continue
					}
					if ev.Key() == tcell.KeyDelete {
						if hasSel {
							deleteInputSel()
							if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
								b.matches = nil
								b.matchIdx = -1
							}
							needsLayout = true
						} else if b.inputCX < len(*targetStr) {
							*targetStr = append((*targetStr)[:b.inputCX], (*targetStr)[b.inputCX+1:]...)
							if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
								b.matches = nil
								b.matchIdx = -1
							}
							needsLayout = true
						}
						continue
					}
					if ev.Key() == tcell.KeyRune && ev.Rune() != 0 {
						deleteInputSel()
						*targetStr = append((*targetStr)[:b.inputCX], append([]rune{ev.Rune()}, (*targetStr)[b.inputCX:]...)...)
						b.inputCX++
						if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
							b.matches = nil
							b.matchIdx = -1
						}
						needsLayout = true
						continue
					}
				}

				if b.searchMode {
					if ev.Key() == tcell.KeyCtrlA && b.isReplace && b.replaceStep == 3 {
						if len(b.searchQuery) > 0 && len(b.matches) > 0 {
							b.replaceAllMatches()
							b.searchMode = false
							b.isReplace = false
							b.replaceStep = 0
							b.clearSelection()
							needsLayout = true
						}
						continue
					}

					if ev.Key() == tcell.KeyUp {
						if len(b.matches) > 0 {
							b.matchIdx = (b.matchIdx - 1 + len(b.matches)) % len(b.matches)
							b.jumpToMatch()
							snapToCursor = true
						}
						continue
					}
					if ev.Key() == tcell.KeyDown {
						if len(b.matches) > 0 {
							b.matchIdx = (b.matchIdx + 1) % len(b.matches)
							b.jumpToMatch()
							snapToCursor = true
						}
						continue
					}
					if ev.Key() == tcell.KeyEnter {
						if b.searchMode && (!b.isReplace || b.replaceStep == 1) && b.matchIdx == -1 {
							b.findAllMatches(editor.cfg.OverlapSearch)
							if len(b.matches) > 0 {
								b.matchIdx = b.findInitialMatchIdx(b.cursor, isShift)
								b.jumpToMatch()
								snapToCursor = true
							}
							if b.isReplace && b.replaceStep == 1 {
								b.replaceStep = 2
								b.inputCX = len(b.replaceQuery)
								b.isInputSelect = false
							}
						} else if isShift {
							if len(b.matches) > 0 {
								b.matchIdx = (b.matchIdx - 1 + len(b.matches)) % len(b.matches)
								b.jumpToMatch()
								snapToCursor = true
							}
						} else {
							if b.isReplace {
								if b.replaceStep == 1 {
									b.replaceStep = 2
									b.inputCX = len(b.replaceQuery)
									b.isInputSelect = false
									if len(b.matches) > 0 {
										b.matchIdx = b.findInitialMatchIdx(b.cursor, false)
										b.jumpToMatch()
										snapToCursor = true
									}
								} else if b.replaceStep == 2 {
									b.replaceStep = 3
									b.findAllMatches(editor.cfg.OverlapSearch)
									if len(b.matches) > 0 {
										b.matchIdx = b.findInitialMatchIdx(b.cursor, false)
										b.jumpToMatch()
										snapToCursor = true
									}
								} else if b.replaceStep == 3 {
									b.replaceCurrent(editor.cfg.OverlapSearch)
									needsLayout = true
									snapToCursor = true
								}
							} else {
								if len(b.matches) > 0 {
									b.matchIdx = (b.matchIdx + 1) % len(b.matches)
									b.jumpToMatch()
									snapToCursor = true
								}
							}
						}
						continue
					}
				}

				if b.gotoMode {
					if ev.Key() == tcell.KeyEnter {
						inputStr := string(b.gotoInput)
						parts := strings.Split(inputStr, ",")
						lineNum, colNum := 0, 0
						fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &lineNum)
						if len(parts) > 1 {
							fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &colNum)
						}
						if lineNum > 0 {
							lineNum--
							if lineNum >= len(b.lines) {
								lineNum = len(b.lines) - 1
							}
							b.cursor.L = lineNum
							targetRuneIdx := colNum - 1
							if targetRuneIdx <= 0 {
								b.cursor.C = 0
							} else {
								byteOffset := 0
								lineData := b.lines[b.cursor.L]
								runeCount := 0
								for byteOffset < len(lineData) && runeCount < targetRuneIdx {
									_, size := utf8.DecodeRune(lineData[byteOffset:])
									byteOffset += size
									runeCount++
								}
								b.cursor.C = byteOffset
							}
						}
						b.gotoMode = false
						snapToCursor = true
						needsLayout = true
						continue
					}
				}
				continue
			}
			// 💡 [여기가 올바른 위치!] 읽기 전용 탭의 텍스트 수정 원천 차단
			if b.isReadOnly {
				k := ev.Key()
				if k == tcell.KeyRune || k == tcell.KeyEnter || k == tcell.KeyBackspace || k == tcell.KeyBackspace2 || k == tcell.KeyDelete || k == tcell.KeyTab {
					continue
				}
				// 💡 줄 이동처럼 내용을 고치는 액션은 어디에 바인딩돼 있든 차단한다.
				if act := editor.bindings[evChord]; act != nil && act.Mutates {
					continue
				}
			}

			// 💡 통합 단축키 디스팩치.
			// 아래 Shift 선택 앵커 블록보다 반드시 먼저 와야 한다 — 그 블록은 modifier 를
			// 보지 않고 Up/Down/Left/... 만 검사하므로, Alt+Up(줄 이동) 같은 액션이
			// 뒤에서 처리되면 없던 clearSelection() 부작용이 붙는다.
			if act := editor.bindings[evChord]; act != nil {
				act.Fn(editor, currentScreen)
				needsLayout = true
				if act.NoSnap {
					snapToCursor = false // 💡 비이동/비편집 단축키의 화면 점프 방지
				}
				continue
			}

			if ev.Key() == tcell.KeyLeft || ev.Key() == tcell.KeyRight || ev.Key() == tcell.KeyUp || ev.Key() == tcell.KeyDown || ev.Key() == tcell.KeyHome || ev.Key() == tcell.KeyEnd || ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyPgDn {
				if !isShift {
					b.clearSelection()
				} else {
					if !b.isSelecting {
						b.isSelecting = true
						b.selection.Start = b.cursor
						// ponytail: snapshot every extra cursor's pre-move position
						// as its own selection anchor, so Shift+move extends each
						// caret's selection independently, mirroring how
						// selection.Start anchors the primary.
						if len(b.extraCursors) > 0 {
							b.extraSelAnchors = append([]Loc(nil), b.extraCursors...)
						}
					}
				}
			}

			switch ev.Key() {
			case tcell.KeyTab, tcell.KeyBacktab:
				b.indentOrDedent(editor.cfg, isShift || ev.Key() == tcell.KeyBacktab)
				needsLayout = true

			case tcell.KeyEscape:
				b.clearSelection()
				if len(b.extraCursors) > 0 {
					b.clearExtraCursors() // ponytail: exit multi-cursor mode
					editor.needsFullRefresh = true
				}
				snapToCursor = false

			case tcell.KeyLeft:
				// ponytail: cursor-count-agnostic (see runMultiCursorMove).
				b.runMultiCursorMove(func() {
					if isCtrl {
						b.moveWordLeft()
					} else {
						if b.cursor.C > 0 {
							_, size := utf8.DecodeLastRune(b.lines[b.cursor.L][:b.cursor.C])
							b.cursor.C -= size
						} else if b.cursor.L > 0 {
							b.cursor.L--
							b.cursor.C = len(b.lines[b.cursor.L])
						}
					}
				})
			case tcell.KeyRight:
				b.runMultiCursorMove(func() {
					if isCtrl {
						b.moveWordRight()
					} else {
						if b.cursor.C < len(b.lines[b.cursor.L]) {
							_, size := utf8.DecodeRune(b.lines[b.cursor.L][b.cursor.C:])
							b.cursor.C += size
						} else if b.cursor.L < len(b.lines)-1 {
							b.cursor.L++
							b.cursor.C = 0
						}
					}
				})
			case tcell.KeyUp:
				b.runMultiCursorMove(func() {
					if isCtrl {
						b.moveParagraphUp()
					} else {
						b.moveCursorVisualLine(-1, editor.cfg, &b.cursor.TargetX)
					}
				})
			case tcell.KeyDown:
				b.runMultiCursorMove(func() {
					if isCtrl {
						b.moveParagraphDown()
					} else {
						b.moveCursorVisualLine(1, editor.cfg, &b.cursor.TargetX)
					}
				})
			case tcell.KeyPgUp:
				b.runMultiCursorMove(func() {
					b.moveCursorVisualLine(-(h - 2), editor.cfg, &b.cursor.TargetX)
				})
			case tcell.KeyPgDn:
				b.runMultiCursorMove(func() {
					b.moveCursorVisualLine(h-2, editor.cfg, &b.cursor.TargetX)
				})
			case tcell.KeyHome:
				if isCtrl {
					b.cursor = Loc{L: 0, C: 0, TargetX: -1}
					if len(b.extraCursors) > 0 {
						b.clearExtraCursors() // ponytail: Ctrl+Home collapses multi-cursor
						editor.needsFullRefresh = true
					}
				} else {
					// ponytail: cursor-count-agnostic, and now soft-wrap-aware
					// for extra cursors too (previously they used a naive
					// line-start shortcut instead of the wrapped sub-line).
					b.runMultiCursorMove(func() {
						cursorSub := b.getCursorSub(editor.cfg)
						currentVL := b.vCache[b.cursor.L][cursorSub]
						b.cursor.C = currentVL.startCX
					})
				}
			case tcell.KeyEnd:
				if isCtrl {
					lastLineIdx := len(b.lines) - 1
					if lastLineIdx < 0 {
						lastLineIdx = 0
					}
					b.cursor = Loc{L: lastLineIdx, C: len(b.lines[lastLineIdx]), TargetX: -1}
					b.stickToWrapEnd = true
					if len(b.extraCursors) > 0 {
						b.clearExtraCursors() // ponytail: Ctrl+End collapses multi-cursor
						editor.needsFullRefresh = true
					}
				} else {
					b.runMultiCursorMove(func() {
						cursorSub := b.getCursorSub(editor.cfg)
						currentVL := b.vCache[b.cursor.L][cursorSub]
						b.cursor.C = currentVL.endCX
					})
					b.stickToWrapEnd = true
				}

			case tcell.KeyEnter:
				// ponytail: cursor-count-agnostic -- runMultiCursorInsert
				// degenerates to the old single-cursor behavior when there
				// are no extra cursors, so there's no separate single path.
				b.BeginTransaction()
				b.DeleteSelection()
				b.runMultiCursorInsert(func(loc Loc, _ bool) string {
					indentStr := ""
					if editor.cfg.AutoIndent {
						line := b.lines[loc.L]
						for j := 0; j < loc.C && j < len(line); j++ {
							if line[j] == ' ' || line[j] == '\t' {
								indentStr += string(line[j])
							} else {
								break
							}
						}
					}
					return "\n" + indentStr
				})
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyBackspace2, tcell.KeyBackspace:
				// ponytail: cursor-count-agnostic (see KeyEnter). A selection
				// still consumes the whole backspace, whether or not extra
				// cursors exist -- extra cursors never carry a selection, so
				// they're untouched either way.
				b.BeginTransaction()
				if !b.DeleteSelection() {
					b.runMultiCursorDelete(func(loc Loc, _ bool) (Loc, Loc) {
						if loc.C > 0 {
							startX := loc.C
							if isCtrl || isAlt {
								b.cursor = loc
								b.moveWordLeft()
								startX = b.cursor.C
								b.cursor = loc
							} else if editor.cfg.SmartBackspace {
								line := b.lines[loc.L]
								isAllSpaces := true
								for i := 0; i < loc.C; i++ {
									if line[i] != ' ' {
										isAllSpaces = false
										break
									}
								}
								if isAllSpaces {
									rem := loc.C % editor.cfg.TabSize
									if rem == 0 {
										rem = editor.cfg.TabSize
									}
									startX -= rem
								} else {
									_, size := utf8.DecodeLastRune(line[:loc.C])
									startX -= size
								}
							} else {
								_, size := utf8.DecodeLastRune(b.lines[loc.L][:loc.C])
								startX -= size
							}
							return Loc{L: loc.L, C: startX, TargetX: -1}, loc
						} else if loc.L > 0 {
							return Loc{L: loc.L - 1, C: len(b.lines[loc.L-1]), TargetX: -1}, loc
						}
						return loc, loc
					})
				}
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyDelete:
				// ponytail: cursor-count-agnostic (see KeyEnter/KeyBackspace).
				b.BeginTransaction()
				if !b.DeleteSelection() {
					b.runMultiCursorDelete(func(loc Loc, _ bool) (Loc, Loc) {
						lineLen := len(b.lines[loc.L])
						if loc.C < lineLen {
							if isCtrl || isAlt {
								b.cursor = loc
								b.moveWordRight()
								endX := b.cursor.C
								b.cursor = loc
								return loc, Loc{L: loc.L, C: endX, TargetX: -1}
							}
							_, size := utf8.DecodeRune(b.lines[loc.L][loc.C:])
							return loc, Loc{L: loc.L, C: loc.C + size, TargetX: -1}
						} else if loc.L < len(b.lines)-1 {
							return loc, Loc{L: loc.L + 1, C: 0, TargetX: -1}
						}
						return loc, loc
					})
				}
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyRune:
				// ponytail: cursor-count-agnostic (see KeyEnter).
				if ev.Rune() != 0 {
					b.BeginTransaction()
					b.DeleteSelection()
					r := ev.Rune()
					b.runMultiCursorInsert(func(Loc, bool) string { return string(r) })
					b.EndTransaction()
					needsLayout = true
				}

			}
			if isShift && b.isSelecting {
				b.selection.End = b.cursor
			}
		}
	}
}
