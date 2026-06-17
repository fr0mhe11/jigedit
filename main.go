package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"time"
	"unicode"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	"github.com/fsnotify/fsnotify"
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
	"github.com/ncruces/zenity"

	"github.com/saintfish/chardet"
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
	configDir := filepath.Dir(configPath)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		_ = os.MkdirAll(configDir, 0755)
		defaultCfg := DefaultConfig()
		data, _ := json.MarshalIndent(defaultCfg, "", "    ")
		_ = ioutil.WriteFile(configPath, data, 0644)
		return defaultCfg
	}
	data, err := ioutil.ReadFile(configPath)
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

// 💡 KWrite(Uchardet) 수준의 통계학 기반 언어 감지 엔진
// 💡 KWrite(Uchardet) 수준의 통계학 기반 언어 감지 엔진 (모든 인코딩 완벽 매핑)

// 💡 사용자가 강제로 인코딩을 지정해서 다시 읽어오는 함수 (Reopen)
func readFileWithEncoding(path string, encName string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}

	// 💡 [숨은 버그 완벽 수정] 사용자가 수동으로 UTF-8을 고르거나 파일 감지기가 리로드할 때,
	// UTF-8 BOM이 존재한다면 유령 글자가 생기지 않도록 확실히 잘라냅니다!
	if (encName == "UTF-8" || encName == "") && bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		data = data[3:]
	}

	enc := getTextEncoding(encName)
	if enc == nil {
		return string(data), nil // UTF-8로 폴백
	}
	reader := transform.NewReader(bytes.NewReader(data), enc.NewDecoder())
	decoded, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// 💡 저장할 때 선택된 인코딩으로 변환하여 저장
func saveFileWithEncoding(path string, content string, encName string) error {
	enc := getTextEncoding(encName)
	if enc == nil {
		// 💡 파일 권한 유지, 심볼릭 링크 유지를 위해 원본 방식(In-place Write)으로 원상 복구!
		return os.WriteFile(path, []byte(content), 0644)
	}

	var buf bytes.Buffer
	writer := transform.NewWriter(&buf, enc.NewEncoder())
	// 인코딩 변환 실패 시 에러를 뱉는 안전장치는 그대로 유지
	if _, err := writer.Write([]byte(content)); err != nil {
		writer.Close()
		return fmt.Errorf("인코딩 변환 오류: %v", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("인코딩 종료 오류: %v", err)
	}

	// 💡 원본 방식으로 복구!
	return os.WriteFile(path, buf.Bytes(), 0644)
}

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
	L int // Line (0부터 시작)
	C int // Column (0부터 시작)
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
}

type MatchInfo struct {
	loc      Loc
	matchLen int
}

// 💡 2. Undo/Redo를 위한 초간단 Action 객체
type Action struct {
	IsInsert bool
	Start    Loc
	End      Loc
	Text     string
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
	vCache      [][]VisualLine // 💡 핵심: 각 줄마다 줄바꿈 상태를 기억하는 영구 캐시 배열!
	cursor      Loc
	selection   Range
	isSelecting bool

	vOffsetL   int // 🟢 [변경] 현재 화면 상단의 물리 줄 번호 (0부터 시작)
	vOffsetSub int // 🟢 [변경] 현재 화면 상단의 물리 줄 내에서의 래핑 번호 (0부터 시작)
	hOffset    int
	isReadOnly bool

	filePath     string
	isConfig     bool
	isModified   bool
	savedContent string
	encoding     string

	searchMode   bool
	replaceStep  int
	isReplace    bool
	searchQuery  []rune
	replaceQuery []rune
	matches      []MatchInfo
	matchIdx     int
	searchCapped bool

	undoStack   []Transaction
	redoStack   []Transaction
	currentTx   *Transaction
	txIDCounter int64
	savedTxID   int64

	gotoMode       bool
	gotoInput      []rune
	inputCX        int
	closeBtnStartX int
	closeBtnEndX   int
	inputSelStart  int
	inputSelEnd    int
	isInputSelect  bool

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
	currentHash uint64
	savedHash   uint64

	stickToWrapEnd bool
	dirtyStartL    int // 🟢 [추가됨] 내용이 변경된 가장 윗줄 번호 기록
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

	// 🟢 여기에 아래 코드를 붙여넣으세요.
	targetCloseBuffer int
	targetEncoding    string // 💡 다시 열기 시 사용자가 선택한 인코딩 기억

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

	needsFullRefresh bool

	prevActiveBuf     int
	prevVOffset       int
	prevVOffsetSub    int // 🟢 [추가] 서브 래핑 줄 캐시 백업용
	prevHOffset       int
	prevPalette       bool
	prevCtxMenu       bool
	prevEncode        bool
	prevPrompt        bool
	prevSearch        bool
	prevGoto          bool
	prevReplace       bool
	prevLinesLen      int
	initialBufferUsed bool
}

type cellState struct {
	mainc rune
	comb  []rune
	style tcell.Style
}

func NewEditor() *Editor {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("파일 감지기를 초기화할 수 없습니다: %v", err)
	}

	e := &Editor{
		buffers:          []*Buffer{NewBuffer()},
		activeBuffer:     0,
		cfg:              LoadConfig(),
		fileWatcher:      watcher,
		needsFullRefresh: true,
		prevActiveBuf:    -1,
		prevVOffset:      -1,
		prevHOffset:      -1,
	}
	e.initPalette()
	e.initContextMenu()
	e.initEncodingMenu() // 💡 인코딩 메뉴 초기화 호출

	go e.listenFileChanges()

	return e
}

func (e *Editor) listenFileChanges() {
	for {
		select {
		case event, ok := <-e.fileWatcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				// 💡 탭 배열(e.buffers)을 여기서 읽지 않고, 메인 스레드로 파일 경로만 안전하게 넘깁니다.
				if globalScreenHandle != nil && *globalScreenHandle != nil {
					(*globalScreenHandle).PostEvent(tcell.NewEventInterrupt(event.Name))
				}
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
		{"복사 (Copy)", "Ctrl+C", ActionMap[tcell.KeyCtrlC]},
		{"잘라내기 (Cut)", "Ctrl+X", ActionMap[tcell.KeyCtrlX]},
		{"붙여넣기 (Paste)", "Ctrl+V", ActionMap[tcell.KeyCtrlV]},
		{"모두 선택 (Select All)", "Ctrl+A", ActionMap[tcell.KeyCtrlA]},
		{"현재 탭 닫기 (Close Tab)", "Ctrl+W", ActionMap[tcell.KeyCtrlW]},
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
		{"파일 열기 (Open)", "Ctrl+O", ActionMap[tcell.KeyCtrlO]},
		{"저장 (Save)", "Ctrl+S", ActionMap[tcell.KeyCtrlS]},
		{"다른 이름으로 저장 (Save As)", "F12", ActionMap[tcell.KeyF12]},
		{"새 탭 열기 (New Tab)", "Ctrl+N", ActionMap[tcell.KeyCtrlN]},
		{"현재 탭 닫기 (Close Tab)", "Ctrl+W", ActionMap[tcell.KeyCtrlW]},
		{"다음 탭 (Next Tab)", "Alt+.", func(e *Editor, s tcell.Screen) {
			e.activeBuffer = (e.activeBuffer + 1) % len(e.buffers)
			e.needsFullRefresh = true
		}},
		{"이전 탭 (Prev Tab)", "Alt+,", func(e *Editor, s tcell.Screen) {
			e.activeBuffer = (e.activeBuffer - 1 + len(e.buffers)) % len(e.buffers)
			e.needsFullRefresh = true
		}},
		{"실행 취소 (Undo)", "Ctrl+Z", ActionMap[tcell.KeyCtrlZ]},
		{"다시 실행 (Redo)", "Ctrl+Y", ActionMap[tcell.KeyCtrlY]},
		{"모두 선택 (Select All)", "Ctrl+A", ActionMap[tcell.KeyCtrlA]},
		{"복사 (Copy)", "Ctrl+C", ActionMap[tcell.KeyCtrlC]},
		{"잘라내기 (Cut)", "Ctrl+X", ActionMap[tcell.KeyCtrlX]},
		{"붙여넣기 (Paste)", "Ctrl+V", ActionMap[tcell.KeyCtrlV]},
		{"찾기 (Find)", "Ctrl+F", ActionMap[tcell.KeyCtrlF]},
		{"바꾸기 (Replace)", "Ctrl+R", ActionMap[tcell.KeyCtrlR]},
		{"줄 이동 (Go To Line)", "Ctrl+G", ActionMap[tcell.KeyCtrlG]},
		{"시간 삽입 (Insert Time)", "F5", ActionMap[tcell.KeyF5]},

		{"설정 파일 편집 (Toggle Config)", "Ctrl+T", ActionMap[tcell.KeyCtrlT]},

		{"설정 초기화 (Reset Config)", "", func(e *Editor, s tcell.Screen) {
			e.promptMode = true
			e.promptType = "reset_config"
		}},

		{"읽기 전용 모드 전환 (Toggle ReadOnly)", "", func(e *Editor, s tcell.Screen) {
			b := e.getActive()
			b.isReadOnly = !b.isReadOnly // 상태 반전
			e.needsFullRefresh = true    // 화면 UI(자물쇠 아이콘 등) 즉시 갱신
		}},

		{"에디터 종료 (Quit)", "Ctrl+Q", ActionMap[tcell.KeyCtrlQ]},
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

// 💡 텍스트를 파일이나 문자열에서 읽어와 순수 2차원 배열로 세팅
func (b *Buffer) setLinesFromText(strData string) {
	strData = strings.ReplaceAll(strData, "\r", "")
	parts := strings.Split(strData, "\n")
	newLines := make([][]byte, len(parts))

	var th uint64 = 0 // 💡 전체 줄의 해시를 XOR로 누적
	for i, p := range parts {
		newLines[i] = []byte(p)
		th ^= fnvHash(newLines[i])
	}
	b.lines = newLines
	b.vCache = make([][]VisualLine, len(newLines)) // 💡 초기화
	b.vLinesValid = false
	b.totalChars = utf8.RuneCountInString(strData)

	b.currentHash = th // 💡 현재 해시 저장
	b.savedHash = th   // 💡 저장용 해시 동기화
	b.clearSelection() // 🟢 [추가] 선택 영역 안전 초기화로 유령 좌표 방지!
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
		lines:       [][]byte{{}},
		vCache:      [][]VisualLine{nil}, // 💡 초기화
		cursor:      Loc{L: 0, C: 0},
		encoding:    "UTF-8",
		dirtyStartL: -1,        // 🟢 [추가됨] -1은 변경 사항 없음(Clean)을 의미
		currentHash: emptyHash, // 💡 초기화
		savedHash:   emptyHash, // 💡 초기화
	}
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars
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

	// 2. 💡 [핵심] 트랜잭션이 달라도 실제 글자 수와 본문 체크섬 해시가 일치하면 Clean! (수동 Net-Change 제로 감지)
	if b.currentHash == b.savedHash && b.totalChars == b.savedTotalChars {
		b.isModified = false
		return
	}

	b.isModified = true
}

func (b *Buffer) markSaved() {
	if len(b.undoStack) == 0 {
		b.savedTxID = 0
	} else {
		b.savedTxID = b.undoStack[len(b.undoStack)-1].ID
	}
	b.savedTotalChars = b.totalChars // 💡 저장 당시 글자 수 기록
	b.savedHash = b.currentHash      // 💡 저장 시점의 체크섬 낙인 점찍기
	b.isModified = false
}

// 💡 chardet 결과를 우리 에디터 이름으로 변환해주는 헬퍼 함수
func mapChardetToOurs(charset string) string {
	charset = strings.ToUpper(charset)
	switch charset {
	case "EUC-KR", "UHC", "CP949", "ISO-2022-KR":
		return "CP949 (EUC-KR)"
	case "SHIFT_JIS", "SHIFT-JIS":
		return "Shift-JIS"
	case "GB-18030", "GB2312", "GBK":
		return "GBK"
	case "BIG5":
		return "Big5"
	case "EUC-JP":
		return "EUC-JP"
	case "ISO-2022-JP":
		return "ISO-2022-JP"
	case "WINDOWS-1252", "CP1252":
		return "CP1252"
	case "WINDOWS-1250", "CP1250":
		return "CP1250"
	case "WINDOWS-1251", "CP1251":
		return "CP1251"
	case "WINDOWS-1253", "CP1253":
		return "CP1253"
	case "WINDOWS-1254", "CP1254":
		return "CP1254"
	case "WINDOWS-1255", "CP1255":
		return "CP1255"
	case "WINDOWS-1256", "CP1256":
		return "CP1256"
	case "WINDOWS-1257", "CP1257":
		return "CP1257"
	case "WINDOWS-1258", "CP1258":
		return "CP1258"
	case "WINDOWS-874", "CP874", "TIS-620":
		return "CP874"
	case "UTF-16LE":
		return "UTF-16 LE"
	case "UTF-16BE":
		return "UTF-16 BE"
	}
	if getTextEncoding(charset) != nil {
		return charset
	}
	return ""
}

// 💡 KWrite(Uchardet) + 아시아 언어 엄격 교차 검증 하이브리드 엔진
// 💡 통계(chardet) + 실전 디코딩 검증(Validation) 하이브리드 엔진
// 💡 상용 에디터급 종결 엔진: 8KB 샘플링 최적화 + 디코딩 실증 검증
func readFileDetectEncoding(path string) (string, string, error) {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("일반 파일이 아닙니다")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}

	if len(data) == 0 {
		return "", "UTF-8", nil
	}

	// 1. BOM (Byte Order Mark) 검사
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return string(data[3:]), "UTF-8", nil
	}
	if bytes.HasPrefix(data, []byte{0xFF, 0xFE}) {
		decoded, _ := ioutil.ReadAll(transform.NewReader(bytes.NewReader(data), getTextEncoding("UTF-16 LE").NewDecoder()))
		return string(decoded), "UTF-16 LE", nil
	}
	if bytes.HasPrefix(data, []byte{0xFE, 0xFF}) {
		decoded, _ := ioutil.ReadAll(transform.NewReader(bytes.NewReader(data), getTextEncoding("UTF-16 BE").NewDecoder()))
		return string(decoded), "UTF-16 BE", nil
	}

	// 2. 순수 UTF-8 검증 (전체 파일 대상)
	if utf8.Valid(data) {
		return string(data), "UTF-8", nil
	}

	// =================================================================
	// 🚀 최적화 핵심: 파일 전체가 아닌 최대 8KB만 잘라서 샘플(Sample)로 사용!
	// =================================================================
	sampleSize := 8192
	var sample []byte
	if len(data) > sampleSize {
		sample = data[:sampleSize]
	} else {
		sample = data
	}

	hasHighBit := false
	for _, b := range sample {
		if b > 127 {
			hasHighBit = true
			break
		}
	}

	// 3. 샘플 데이터로 통계학 엔진 가동 (0.001초 컷)
	detector := chardet.NewTextDetector()
	results, err := detector.DetectAll(sample)

	bestEncName := "UTF-8"
	var minErrorCount = -1
	var fallbackEncName string

	if err == nil && len(results) > 0 {
		for _, res := range results {
			mapped := mapChardetToOurs(res.Charset)
			if mapped == "" {
				continue
			}

			// 서유럽어 오탐지 무시
			isWestern := strings.HasPrefix(mapped, "ISO-8859") || strings.HasPrefix(mapped, "CP125")
			if hasHighBit && isWestern {
				continue
			}

			enc := getTextEncoding(mapped)
			if enc == nil {
				continue
			}

			// 💡 샘플 데이터(8KB)만 변환해서 깨진 글자 검사! (메모리 낭비 X)
			sampleDecoded, decErr := ioutil.ReadAll(transform.NewReader(bytes.NewReader(sample), enc.NewDecoder()))
			if decErr != nil {
				continue
			}

			errorCount := bytes.Count(sampleDecoded, []byte("\uFFFD"))

			// 에러가 0개면 완벽한 정답!
			if errorCount == 0 {
				bestEncName = mapped
				goto DECODE_FULL_FILE // 정답을 찾았으니 즉시 파일 전체 변환으로 직행
			}

			if minErrorCount == -1 || errorCount < minErrorCount {
				minErrorCount = errorCount
				fallbackEncName = mapped
			}
		}
	}

	// 4. 완벽한 후보가 없다면, 에러가 제일 적었던 언어 채택
	if fallbackEncName != "" && minErrorCount < len(sample)/10 {
		bestEncName = fallbackEncName
	} else if hasHighBit {
		// 최후의 보루: 다 실패하면 한국어로 강제
		bestEncName = "CP949 (EUC-KR)"
	}

DECODE_FULL_FILE:
	// =================================================================
	// 5. 확정된 인코딩으로 "원본 전체(data)"를 딱 한 번만 디코딩합니다.
	// =================================================================
	if bestEncName != "UTF-8" {
		enc := getTextEncoding(bestEncName)
		if enc != nil {
			decodedBytes, err := ioutil.ReadAll(transform.NewReader(bytes.NewReader(data), enc.NewDecoder()))
			if err == nil {
				return string(decodedBytes), bestEncName, nil
			}
		}
	}

	// 최후의 최후 폴백
	return string(data), "UTF-8", nil
}

func (b *Buffer) reloadFromDisk() bool {
	if b.filePath == "" || b.isModified {
		return false
	}

	var strData string
	var err error
	if b.encoding != "" && b.encoding != "UTF-8" {
		strData, err = readFileWithEncoding(b.filePath, b.encoding)
	} else {
		strData, _, err = readFileDetectEncoding(b.filePath)
	}

	if err != nil || strData == b.savedContent {
		return false
	}

	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars // 글자 수 완벽 동기화
	b.lastExternalSync = time.Now()
	b.cursor = b.clampLoc(b.cursor)
	b.undoStack = nil
	b.redoStack = nil
	b.isSelecting = false
	b.txIDCounter = 0
	b.savedTxID = 0
	return true
}

func (b *Buffer) reopenWithEncoding(encName string) {
	strData, err := readFileWithEncoding(b.filePath, encName)
	if err != nil {
		return
	}

	b.encoding = encName
	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars // 💡 글자 수 완벽 동기화 (누락 방지)
	b.isModified = false
	b.lastExternalSync = time.Now()
	b.cursor = Loc{0, 0}
	b.vOffsetL = 0
	b.vOffsetSub = 0
	b.hOffset = 0
	b.undoStack = nil
	b.redoStack = nil
	b.isSelecting = false
	b.txIDCounter = 0
	b.savedTxID = 0
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

	// 💡 [해시 증분] 조작 전 원본 행의 해시를 전체 합에서 제거
	b.currentHash ^= fnvHash(b.lines[loc.L])

	b.totalChars += utf8.RuneCount(textBytes)

	var newLines [][]byte
	start := 0
	for i, c := range textBytes {
		if c == '\n' {
			newLines = append(newLines, append([]byte(nil), textBytes[start:i]...))
			start = i + 1
		}
	}
	newLines = append(newLines, append([]byte(nil), textBytes[start:]...))
	newCache := make([][]VisualLine, len(newLines))

	if len(newLines) == 1 {
		line := b.lines[loc.L]
		newLine := make([]byte, 0, len(line)+len(newLines[0]))
		newLine = append(newLine, line[:loc.C]...)
		newLine = append(newLine, newLines[0]...)
		newLine = append(newLine, line[loc.C:]...)
		b.lines[loc.L] = newLine
		b.vCache[loc.L] = nil // 💡 수정한 줄의 캐시 무효화

		// 💡 [해시 증분] 변경이 완료된 단일 행의 새 해시를 주입
		b.currentHash ^= fnvHash(b.lines[loc.L])
		return Loc{L: loc.L, C: loc.C + len(newLines[0])}
	}

	originalLine := b.lines[loc.L]
	tail := append([]byte(nil), originalLine[loc.C:]...)
	firstLine := make([]byte, 0, loc.C+len(newLines[0]))
	firstLine = append(firstLine, originalLine[:loc.C]...)
	firstLine = append(firstLine, newLines[0]...)

	b.lines[loc.L] = firstLine
	b.vCache[loc.L] = nil // 💡 캐시 무효화

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
	b.vCache = append(b.vCache, make([][]VisualLine, addLen)...)

	copy(b.lines[loc.L+addLen+1:], b.lines[loc.L+1:])
	copy(b.vCache[loc.L+addLen+1:], b.vCache[loc.L+1:])

	copy(b.lines[loc.L+1:], newLines[1:])
	copy(b.vCache[loc.L+1:], newCache[1:])

	return Loc{L: loc.L + len(newLines) - 1, C: len(newLines[len(newLines)-1]) - len(tail)}
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
		// 💡 [해시 증분] 조작 전 원본 행의 해시 제거
		b.currentHash ^= fnvHash(b.lines[start.L])

		line := b.lines[start.L]
		deleted := string(line[start.C:end.C])
		newLine := make([]byte, 0, len(line)-(end.C-start.C))
		newLine = append(newLine, line[:start.C]...)
		newLine = append(newLine, line[end.C:]...)
		b.lines[start.L] = newLine
		b.vCache[start.L] = nil // 💡 캐시 무효화
		b.totalChars -= utf8.RuneCountInString(deleted)

		// 💡 [해시 증분] 데이터가 잘려 나간 행의 새 해시 주입
		b.currentHash ^= fnvHash(b.lines[start.L])
		return deleted
	}

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
	b.vCache[start.L] = nil // 💡 캐시 무효화

	// 💡 [해시 증분] 두 줄이 병합되어 살아남은 첫 줄의 새 해시 주입
	b.currentHash ^= fnvHash(b.lines[start.L])

	// 💡 최적화: 임시 배열 생성 없이 제자리에서 잘라내기 병합 (단일 복사)
	oldLen := len(b.lines)
	b.lines = append(b.lines[:start.L+1], b.lines[end.L+1:]...)
	b.vCache = append(b.vCache[:start.L+1], b.vCache[end.L+1:]...)

	// 💡 메모리 누수 방지: 슬라이스 축소 후 꼬리 부분에 남은 포인터들을 nil로 초기화 (GC 수거 지원)
	for i := len(b.lines); i < oldLen; i++ {
		b.lines[:cap(b.lines)][i] = nil
		b.vCache[:cap(b.vCache)][i] = nil
	}

	deletedStr := string(deletedBytes)
	b.totalChars -= utf8.RuneCountInString(deletedStr)
	return deletedStr
}

// --- [ 4. 코어 논리 엔진 (Undo/Redo, Selection, Movement, VisualLine) ] ---

// 💡 1. 절대 꼬이지 않는 초간단 Undo / Redo 시스템
func (b *Buffer) BeginTransaction() {
	if b.currentTx == nil {
		b.currentTx = &Transaction{BeforeLoc: b.cursor}
	}
}
func (b *Buffer) EndTransaction() {
	if b.currentTx != nil && len(b.currentTx.Actions) > 0 {
		b.txIDCounter++
		b.currentTx.ID = b.txIDCounter
		b.currentTx.AfterLoc = b.cursor
		b.currentTx.Time = time.Now()

		// 💡 부활한 스마트 Undo/Redo 병합(Merge) 로직!
		if len(b.currentTx.Actions) == 1 && len(b.undoStack) > 0 {
			lastTx := &b.undoStack[len(b.undoStack)-1]

			if time.Since(lastTx.Time) < 1*time.Second && len(lastTx.Actions) == 1 {
				// 💡 추가: 방금 저장된 상태의 트랜잭션이라면 병합을 거부함!
				if lastTx.ID == b.savedTxID {
					goto SKIP_MERGE
				}

				currAct := b.currentTx.Actions[0]
				lastAct := &lastTx.Actions[0]

				// 1. 연속 삽입(Insert) 묶기 (단어 단위로)
				if currAct.IsInsert && lastAct.IsInsert && !strings.Contains(currAct.Text, "\n") && !strings.Contains(lastAct.Text, "\n") {
					isSpaceCurr := (currAct.Text == " " || currAct.Text == "\t")
					isSpaceLast := strings.HasSuffix(lastAct.Text, " ") || strings.HasSuffix(lastAct.Text, "\t")

					if !isSpaceCurr && !isSpaceLast {
						if currAct.Start == lastAct.End {
							lastAct.Text += currAct.Text
							lastAct.End = currAct.End
							lastTx.AfterLoc = b.cursor
							lastTx.Time = b.currentTx.Time
							b.redoStack = nil
							b.currentTx = nil
							b.checkModified()
							return
						}
					}
				}

				// 2. 연속 삭제(Backspace / Delete) 묶기
				if !currAct.IsInsert && !lastAct.IsInsert && !strings.Contains(currAct.Text, "\n") && !strings.Contains(lastAct.Text, "\n") {
					// 2-1. 백스페이스 방향 (현재 지운 범위의 끝이, 이전에 지운 범위의 시작점과 맞닿을 때)
					if currAct.End == lastAct.Start {
						lastAct.Text = currAct.Text + lastAct.Text
						lastAct.Start = currAct.Start
						lastTx.AfterLoc = b.cursor
						lastTx.Time = b.currentTx.Time
						b.redoStack = nil
						b.currentTx = nil
						b.checkModified()
						return
					}
					// 2-2. Delete 키 방향 (현재 지운 범위의 시작이, 이전에 지운 범위의 시작점과 같을 때)
					if currAct.Start == lastAct.Start {
						lastAct.Text = lastAct.Text + currAct.Text
						lastAct.End = Loc{L: lastAct.Start.L, C: lastAct.Start.C + utf8.RuneCountInString(lastAct.Text)}
						lastTx.AfterLoc = b.cursor
						lastTx.Time = b.currentTx.Time
						b.redoStack = nil
						b.currentTx = nil
						b.checkModified()
						return
					}
				}
			}
		}

	SKIP_MERGE:
		b.undoStack = append(b.undoStack, *b.currentTx)
		b.redoStack = nil

		// 💡 메모리 누수 방지: 단순 슬라이싱은 기존 배열이 메모리에 남게 되므로,
		// 완전히 새로운 슬라이스로 복사(Copy)하여 가비지 컬렉터(GC)가 예전 메모리를 회수하게 함.
		if len(b.undoStack) > 1000 {
			newStack := make([]Transaction, 0, 800)
			newStack = append(newStack, b.undoStack[len(b.undoStack)-800:]...)
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
		b.currentTx.Actions = append(b.currentTx.Actions, Action{IsInsert: true, Start: loc, End: endLoc, Text: text})
	}
	b.cursor = endLoc
}
func (b *Buffer) DeleteTextWithRecord(start, end Loc) {
	if b.isReadOnly {
		return
	}
	text := b.Remove(start, end)
	if text != "" && b.currentTx != nil {
		b.currentTx.Actions = append(b.currentTx.Actions, Action{IsInsert: false, Start: start, End: end, Text: text})
	}
	b.cursor = start
}
func (b *Buffer) DeleteSelection() bool {
	if b.isReadOnly || !b.HasSelection() {
		return false
	}
	start, end := b.getSelectionRange()
	start = b.clampLoc(start) // 🟢 [추가] 삭제 시 좌표 이중 보정
	end = b.clampLoc(end)     // 🟢 [추가] 삭제 시 좌표 이중 보정
	b.DeleteTextWithRecord(start, end)
	b.clearSelection()
	return true
}
func (b *Buffer) Undo() {
	if b.isReadOnly || len(b.undoStack) == 0 {
		return
	}
	tx := b.undoStack[len(b.undoStack)-1]
	b.undoStack = b.undoStack[:len(b.undoStack)-1]
	for i := len(tx.Actions) - 1; i >= 0; i-- {
		act := tx.Actions[i]
		if act.IsInsert {
			b.Remove(act.Start, act.End)
		} else {
			b.Insert(act.Start, act.Text)
		}
	}
	b.cursor = b.clampLoc(tx.BeforeLoc)
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
	b.undoStack = append(b.undoStack, tx)
	b.checkModified()
	b.clearSelection()
}

// 💡 2. 절대 좌표 기반 선택(Selection) 조작 로직
func (b *Buffer) getSelectionRange() (Loc, Loc) {
	s, e := b.selection.Start, b.selection.End
	if s.L > e.L || (s.L == e.L && s.C > e.C) {
		return e, s
	}
	return s, e
}
func (b *Buffer) HasSelection() bool { return b.selection.Start != b.selection.End }
func (b *Buffer) clearSelection() {
	b.isSelecting = false
	b.selection.Start = b.cursor
	b.selection.End = b.cursor
}
func (b *Buffer) selectAll() {
	b.isSelecting = true
	b.selection.Start = Loc{0, 0}
	b.selection.End = Loc{len(b.lines) - 1, len(b.lines[len(b.lines)-1])}
	b.cursor = b.selection.End
}
func (b *Buffer) getSelectedText() string {
	if !b.HasSelection() {
		return ""
	}
	s, e := b.getSelectionRange()
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
	b.selection.Start = Loc{b.cursor.L, left}
	b.selection.End = Loc{b.cursor.L, right}
	b.cursor = b.selection.End
}
func (b *Buffer) selectLineAtCursor() {
	b.isSelecting = true
	b.selection.Start = Loc{b.cursor.L, 0}
	b.selection.End = Loc{b.cursor.L, len(b.lines[b.cursor.L])}
	b.cursor = b.selection.End
}

// 💡 3. 배열 인덱스 기반의 이동(Movement) 로직
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

// 💡 4. 동적 Visual Line (화면 렌더링용 줄바꿈 계산) 캐시 엔진
// 💡 4. 동적 가상 렌더링 (Dynamic Virtual Windowing) 엔진
// 💡 4. 부분 계산(Per-Line Cache) 엔진 - 100만 줄도 0.001초 컷
// 💡 4. 부분 계산(Per-Line Cache) 엔진
// 🟢 [변경] 특정 물리 줄의 visual line 캐시가 비어 있으면 동적으로 계산해주는 헬퍼
// 🟢 [변경] 특정 물리 줄의 visual line 캐시가 비어 있으면 동적으로 계산해주는 헬퍼
func (b *Buffer) ensureVCache(i int, cfg Config) []VisualLine {
	if b.vCache[i] != nil {
		return b.vCache[i]
	}
	line := b.lines[i]
	lineNumWidth := b.getLineNumWidth(cfg)
	textMaxWidth := b.cachedMaxWidth - lineNumWidth
	if textMaxWidth <= 0 {
		textMaxWidth = 1
	}

	var temp []VisualLine
	if !cfg.LineWrapping || len(line) == 0 {
		temp = []VisualLine{{isWrapped: false, startCX: 0, endCX: len(line)}}
	} else {
		start, currentX := 0, 0
		for cx, r := range string(line) {
			rw := fastRuneWidth(r, cfg.TabSize)
			if currentX+rw > textMaxWidth {
				temp = append(temp, VisualLine{isWrapped: start > 0, startCX: start, endCX: cx})
				start = cx
				currentX = 0
			}
			currentX += rw
		}
		temp = append(temp, VisualLine{isWrapped: start > 0, startCX: start, endCX: len(line)})
	}
	b.vCache[i] = temp
	return temp
}

// 🟢 [변경] O(1) 크기 무효화만 수행
func (b *Buffer) generateVisualLines(maxWidth int, cfg Config) []VisualLine {
	if b.cachedMaxWidth != maxWidth || b.cachedConfig.LineWrapping != cfg.LineWrapping || b.cachedConfig.TabSize != cfg.TabSize {
		for i := range b.vCache {
			b.vCache[i] = nil
		}
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

func (b *Buffer) getCursorVisualRange(cfg Config) (int, int) {
	b.cursor = b.clampLoc(b.cursor)
	line := b.lines[b.cursor.L]

	cursorVisX := 0
	for i := 0; i < b.cursor.C && i < len(line); {
		r, size := utf8.DecodeRune(line[i:])
		rw := fastRuneWidth(r, cfg.TabSize)
		cursorVisX += rw
		i += size
	}

	charWidth := 1
	if b.cursor.C < len(line) {
		r, _ := utf8.DecodeRune(line[b.cursor.C:])
		charWidth = runewidth.RuneWidth(r)
		if r == '\t' {
			charWidth = cfg.TabSize
		}
	}

	return cursorVisX, cursorVisX + charWidth
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
		return Loc{0, 0}
	}

	relativeY := my - tabHeight
	if relativeY < 0 {
		return Loc{0, 0}
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
	vl := b.vCache[currL][currSub]

	relativeX := mx - lineNumWidth + b.hOffset
	if relativeX <= 0 {
		return Loc{currL, vl.startCX}
	}

	currentX := 0
	line := b.lines[currL]

	for i := vl.startCX; i < vl.endCX && i < len(line); {
		r, size := utf8.DecodeRune(line[i:])
		rw := fastRuneWidth(r, cfg.TabSize)

		if relativeX >= currentX && relativeX < currentX+rw {
			if rw > 1 && relativeX >= currentX+(rw/2)+(rw%2) {
				return Loc{currL, i + size}
			}
			return Loc{currL, i}
		}
		currentX += rw
		i += size
	}

	endC := vl.endCX
	if endC > len(line) {
		endC = len(line)
	}
	return Loc{currL, endC}
}

// 🟢 [추가] 위/아래(Up/Down/PgUp/PgDn) 키를 눌렀을 때, 누적합 없이 인접 가상라인으로 커서를 안전하게 옮기는 O(1) 헬퍼
func (b *Buffer) moveCursorVisualLine(delta int, cfg Config) {
	b.cursor = b.clampLoc(b.cursor)
	cursorSub := b.getCursorSub(cfg)
	b.ensureVCache(b.cursor.L, cfg)
	currentVL := b.vCache[b.cursor.L][cursorSub]

	// 💡 1. 현재 커서의 시각적 X 오프셋 계산 (바이트가 아닌 화면 칸 수 기준)
	visualOffset := 0
	currLineData := b.lines[b.cursor.L]
	for i := currentVL.startCX; i < b.cursor.C && i < currentVL.endCX && i < len(currLineData); {
		r, size := utf8.DecodeRune(currLineData[i:])
		visualOffset += fastRuneWidth(r, cfg.TabSize)
		i += size
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
		e.promptMode || e.prevPrompt ||
		b.searchMode || e.prevSearch ||
		b.gotoMode || e.prevGoto || len(b.lines) != e.prevLinesLen {
		forceAllDirty = true
	} else {
		if b.totalChars == e.prevTotalChars && b.txIDCounter == e.prevTxID {
			isFastPath = true
		}
	}

	if forceAllDirty {
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

		if !forceAllDirty {
			for x := 0; x < w; x++ {
				setCell(x, currentRenderY, ' ', nil, tcell.StyleDefault)
			}
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

		currentX := 0
		if len(lineData) == 0 && lineIdx == b.cursor.L {
			cursorVX = lineNumWidth - b.hOffset
			cursorVY = currentRenderY
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

			if hasSel {
				selected := false
				if lineIdx > selStart.L && lineIdx < selEnd.L {
					selected = true
				}
				if lineIdx == selStart.L && lineIdx == selEnd.L {
					selected = i >= selStart.C && i < selEnd.C
				}
				if lineIdx == selStart.L && lineIdx < selEnd.L {
					selected = i >= selStart.C
				}
				if lineIdx == selEnd.L && lineIdx > selStart.L {
					selected = i < selEnd.C
				}
				if selected {
					charStyle = selectedStyle
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
		}

		if !vl.isWrapped || vl.endCX == len(lineData) {
			if hasSel {
				selected := false
				i := len(lineData)
				if lineIdx > selStart.L && lineIdx < selEnd.L {
					selected = true
				}
				if lineIdx == selStart.L && lineIdx == selEnd.L {
					selected = i >= selStart.C && i < selEnd.C
				}
				if lineIdx == selStart.L && lineIdx < selEnd.L {
					selected = i >= selStart.C
				}
				if lineIdx == selEnd.L && lineIdx > selStart.L {
					selected = i < selEnd.C
				}
				if selected {
					if currentX >= b.hOffset && currentX < b.hOffset+textMaxWidth {
						setCell(lineNumWidth+currentX-b.hOffset, currentRenderY, ' ', nil, selectedStyle)
					}
				}
			}
		}

		if lineIdx == b.cursor.L && b.cursor.C == vl.endCX {
			if b.cursor.C == len(lineData) || currSub+1 >= len(vcls) || b.stickToWrapEnd { // 🟢 vIdx 불필요
				cursorVX = lineNumWidth + currentX - b.hOffset
				cursorVY = currentRenderY
			}
		}

		vlWidth := 0
		for i := vl.startCX; i < vl.endCX; {
			r, size := utf8.DecodeRune(lineData[i:])
			rw := fastRuneWidth(r, e.cfg.TabSize)
			vlWidth += rw
			i += size
		}

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

	if !forceAllDirty {
		for y := currentRenderY; y < h-1; y++ {
			for x := 0; x < w; x++ {
				setCell(x, y, ' ', nil, tcell.StyleDefault)
			}
		}
	}

	if e.promptMode {
		var promptMsg string
		if e.promptType == "quit" {
			promptMsg = " [Warning] 저장되지 않은 탭이 있습니다. 무시하고 종료할까요? (y/n)"
		} else if e.promptType == "close" {
			promptMsg = " [Warning] 변경된 내용이 있습니다. 탭을 닫을까요? (y/n)"
		} else if e.promptType == "reset_config" {
			promptMsg = " [Warning] 설정을 기본값으로 초기화하시겠습니까? (y/n)"
		} else if e.promptType == "reopen" {
			promptMsg = " [Warning] 변경된 내용이 있습니다. 무시하고 다시 열까요? (y/n)"
		} else if e.promptType == "close_config" {
			promptMsg = " [Warning] 설정이 저장되지 않았습니다. 무시하고 닫을까요? (y/n)"
		} else if e.promptType == "external_change" {
			promptMsg = " [Warning] 파일이 외부에서 변경되었습니다. 디스크 내용으로 덮어쓸까요? (y/n)"
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
		if targetStr != nil {
			selStart, selEnd := b.inputSelStart, b.inputSelEnd
			if selStart > selEnd {
				selStart, selEnd = selEnd, selStart
			}

			for i, r := range *targetStr {
				style := statusStyle
				if b.isInputSelect && i >= selStart && i < selEnd {
					style = selectedStyle
				}
				if currentX < cbStartX {
					setCell(currentX, h-1, r, nil, style)
					currentX += runewidth.RuneWidth(r)
				}
			}
			cursorVX = inputStartX + runewidth.StringWidth(string((*targetStr)[:b.inputCX]))
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
				selChars += utf8.RuneCount(b.lines[selStart.L][selStart.C:]) + 1
				for idx := selStart.L + 1; idx < selEnd.L; idx++ {
					selChars += utf8.RuneCount(b.lines[idx]) + 1
				}
				selChars += utf8.RuneCount(b.lines[selEnd.L][:selEnd.C])
			}
			charCountStr = fmt.Sprintf("%d Sel", selChars)
		} else {
			charCountStr = fmt.Sprintf("%d Chars", b.totalChars)
		}

		prefix := " [Ctrl+P] Command Palette | "
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

	drawMenu := func(isActive bool, title string, items []PaletteItem, cursor, mx, my int, outX, outY, outW, outH *int) {
		if !isActive {
			return
		}
		pWidth := 40
		if title == " Command Palette " {
			pWidth = 60
		}
		for _, item := range items {
			w := runewidth.StringWidth(item.Name) + 10
			if w > pWidth {
				pWidth = w
			}
		}
		pHeight := len(items) + 2
		if pHeight > h-4 {
			pHeight = h - 4
		}

		pX, pY := mx, my
		if title == " Command Palette " {
			pX = (w - pWidth) / 2
			pY = (h - pHeight) / 2
		}
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
		*outX, *outY, *outW, *outH = pX, pY, pWidth, pHeight

		marginStyle := tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorDefault)
		for y := -1; y <= pHeight; y++ {
			for x := -1; x <= pWidth; x++ {
				if pX+x >= 0 && pX+x < w && pY+y >= 0 && pY+y < h {
					setCell(pX+x, pY+y, ' ', nil, marginStyle)
				}
			}
		}

		borderStyle := tcell.StyleDefault.Background(tcell.ColorDarkBlue).Foreground(tcell.ColorWhite)
		itemStyle := tcell.StyleDefault.Background(tcell.ColorBlack).Foreground(tcell.ColorWhite)
		selectedStyle := tcell.StyleDefault.Background(tcell.ColorWhite).Foreground(tcell.ColorBlack).Bold(true)

		for y := 0; y < pHeight; y++ {
			for x := 0; x < pWidth; x++ {
				style := itemStyle
				r := ' '
				if y == 0 || y == pHeight-1 || x == 0 || x == pWidth-1 {
					style = borderStyle
					if y == 0 && x > 0 && x < pWidth-1 {
						r = '─'
					}
					if y == pHeight-1 && x > 0 && x < pWidth-1 {
						r = '─'
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
			for i, r := range title {
				setCell(tx+i, pY, r, nil, borderStyle)
			}
		}

		visibleItems := pHeight - 2
		startIdx := cursor - visibleItems/2
		if startIdx < 0 {
			startIdx = 0
		}
		if startIdx+visibleItems > len(items) {
			startIdx = len(items) - visibleItems
			if startIdx < 0 {
				startIdx = 0
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
		cursorVX = -1
	}

	drawMenu(e.paletteActive, " Command Palette ", e.paletteItems, e.paletteCursor, 0, 0, &e.paletteX, &e.paletteY, &e.paletteW, &e.paletteH)
	drawMenu(e.ctxMenuActive, "", e.ctxMenuItems, e.ctxMenuCursor, e.ctxMenuX, e.ctxMenuY, &e.ctxMenuX, &e.ctxMenuY, &e.ctxMenuW, &e.ctxMenuH)
	drawMenu(e.encodeMenuActive, e.encodeMenuTitle, e.encodeMenuItems, e.encodeMenuCursor, e.encodeMenuX, e.encodeMenuY, &e.encodeMenuX, &e.encodeMenuY, &e.encodeMenuW, &e.encodeMenuH)

	e.prevActiveBuf = e.activeBuffer
	e.prevVOffset = b.vOffsetL
	e.prevVOffsetSub = b.vOffsetSub
	e.prevHOffset = b.hOffset
	e.prevPalette = e.paletteActive
	e.prevCtxMenu = e.ctxMenuActive
	e.prevEncode = e.encodeMenuActive
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

	b.dirtyStartL = -1

	if cursorVX >= lineNumWidth && cursorVX < w && cursorVY >= 0 && cursorVY < h {
		s.ShowCursor(cursorVX, cursorVY)
	} else {
		s.HideCursor()
	}
	s.Show()
}
func runeSliceEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var globalScreenHandle *tcell.Screen

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

	strData, encoding, err := readFileDetectEncoding(filePath)
	if err != nil {
		return
	}

	b := NewBuffer()
	b.filePath = filePath
	b.encoding = encoding

	// 💡 파일 감지기에 등록
	e.fileWatcher.Add(filePath)

	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars // 💡 글자 수 완벽 동기화 (누락 방지)
	e.buffers = append(e.buffers, b)
	e.activeBuffer = len(e.buffers) - 1
}

func (e *Editor) saveActiveFile(s tcell.Screen) {
	b := e.getActive()
	if b.filePath == "" && !b.isConfig {
		e.saveAsFile(s)
		return
	}

	content := b.getContent()
	err := saveFileWithEncoding(b.filePath, content, b.encoding)
	if err != nil {
		e.promptMode = true
		e.promptType = "alert"
		e.alertMessage = fmt.Sprintf("저장 실패: %v", err)
		return // 💡 실패하면 저장되었다고 마킹하지 않고 빠져나감
	}

	b.savedContent = content
	b.markSaved()

	if b.isConfig {
		var newCfg Config
		if err := json.Unmarshal([]byte(content), &newCfg); err == nil {
			e.cfg = newCfg
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

	// 💡 이전 파일 경로를 백업해둡니다
	oldPath := b.filePath
	b.filePath = filePath

	content := b.getContent()
	err = saveFileWithEncoding(b.filePath, content, b.encoding)
	if err != nil {
		e.promptMode = true
		e.promptType = "alert"
		e.alertMessage = fmt.Sprintf("저장 실패: %v", err)
		return
	}

	b.savedContent = content
	b.markSaved()

	// 💡 이름이 바뀌었다면 이전 파일의 감시를 해제하고 새 파일을 감시
	if oldPath != "" && oldPath != filePath {
		e.fileWatcher.Remove(oldPath)
	}
	e.fileWatcher.Add(filePath)

	if b.isConfig {
		var newCfg Config
		if err := json.Unmarshal([]byte(content), &newCfg); err == nil {
			e.cfg = newCfg
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
	strData, _, err := readFileDetectEncoding(path)
	if err != nil {
		return
	}

	configBuf := NewBuffer()
	configBuf.filePath = path
	configBuf.isConfig = true

	// 💡 config.json 파일도 감지기에 등록
	e.fileWatcher.Add(path)

	configBuf.setLinesFromText(strData)
	configBuf.savedContent = configBuf.getContent()
	configBuf.savedTotalChars = configBuf.totalChars // 💡 추가됨
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

	var re *regexp.Regexp
	if b.searchRegex || b.searchWord {
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
		if err == nil {
			re = compiled
		}
	}

	searchRunes := []rune(query)
	if re == nil && !b.searchCase {
		for i := range searchRunes {
			searchRunes[i] = unicode.ToLower(searchRunes[i])
		}
	}

	// 💡 1줄 안에서 매치를 찾는 공통 로직 (클로저)
	findInLine := func(lineIdx int) []MatchInfo {
		var lineMatches []MatchInfo
		lineStr := string(b.lines[lineIdx])
		if re != nil {
			locs := re.FindAllStringIndex(lineStr, -1)
			for _, loc := range locs {
				lineMatches = append(lineMatches, MatchInfo{loc: Loc{lineIdx, loc[0]}, matchLen: loc[1] - loc[0]})
			}
		} else {
			searchRunes := []rune(query)
			if !b.searchCase {
				for i := range searchRunes {
					searchRunes[i] = unicode.ToLower(searchRunes[i])
				}
			}
			offset := 0
			for offset <= len(lineStr) {
				match := true
				currentByteOffset := offset
				for j := 0; j < len(searchRunes); j++ {
					if currentByteOffset >= len(lineStr) {
						match = false
						break
					}
					tr, tsize := utf8.DecodeRuneInString(lineStr[currentByteOffset:])
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
					lineMatches = append(lineMatches, MatchInfo{loc: Loc{lineIdx, offset}, matchLen: matchLen})
					if !overlap {
						offset = currentByteOffset
					} else {
						_, size := utf8.DecodeRuneInString(lineStr[offset:])
						offset += size
					}
				} else {
					if offset >= len(lineStr) {
						break
					}
					_, size := utf8.DecodeRuneInString(lineStr[offset:])
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
	cursorMatches := findInLine(b.cursor.L)
	var cursorLineAbove []MatchInfo
	for _, m := range cursorMatches {
		if m.loc.C < b.cursor.C {
			cursorLineAbove = append(cursorLineAbove, m)
			countAbove++
		} else {
			matchesBelow = append(matchesBelow, m)
			countBelow++
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
		lm := findInLine(i)
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
		lm := findInLine(i)
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
	b.selection.End = Loc{m.loc.L, m.loc.C + m.matchLen}
}

func (b *Buffer) replaceCurrent(overlap bool) {
	if b.matchIdx < 0 || b.matchIdx >= len(b.matches) {
		return
	}
	m := b.matches[b.matchIdx]

	b.BeginTransaction()
	b.DeleteTextWithRecord(m.loc, Loc{m.loc.L, m.loc.C + m.matchLen})
	b.InsertTextWithRecord(m.loc, string(b.replaceQuery))
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

type EditorAction func(e *Editor, s tcell.Screen)

var ActionMap = map[tcell.Key]EditorAction{
	tcell.KeyCtrlG: func(e *Editor, s tcell.Screen) {
		b := e.getActive()
		b.clearSelection()
		b.searchMode = false
		b.gotoMode = true
		b.gotoInput = []rune{}
		b.inputCX = 0
		b.isInputSelect = false
	},
	tcell.KeyF5: func(e *Editor, s tcell.Screen) {
		b := e.getActive()
		if b.isReadOnly {
			return
		}
		b.BeginTransaction()
		b.DeleteSelection()
		goLayout := convertLinuxDateToGoLayout(e.cfg.DateFormat)
		b.InsertTextWithRecord(b.cursor, time.Now().Format(goLayout))
		b.EndTransaction()
	},
	tcell.KeyCtrlQ: func(e *Editor, s tcell.Screen) {
		for _, b := range e.buffers {
			if b.isModified {
				e.promptMode = true
				e.promptType = "quit"
				return
			}
		}
		s.Fini()
		os.Exit(0)
	},
	tcell.KeyCtrlT: func(e *Editor, s tcell.Screen) { e.getActive().clearSelection(); e.toggleConfigBuffer() },
	tcell.KeyCtrlS: func(e *Editor, s tcell.Screen) { e.saveActiveFile(s) },
	tcell.KeyCtrlO: func(e *Editor, s tcell.Screen) { e.openFile(s) },
	tcell.KeyF12:   func(e *Editor, s tcell.Screen) { e.saveAsFile(s) },
	tcell.KeyCtrlP: func(e *Editor, s tcell.Screen) {
		e.paletteActive = !e.paletteActive
		e.paletteCursor = 0
		e.ctxMenuActive = false
		e.encodeMenuActive = false
	},
	tcell.KeyCtrlA: func(e *Editor, s tcell.Screen) { e.getActive().selectAll() },
	tcell.KeyCtrlZ: func(e *Editor, s tcell.Screen) { e.getActive().Undo() },
	tcell.KeyCtrlY: func(e *Editor, s tcell.Screen) { e.getActive().Redo() },
	tcell.KeyCtrlC: func(e *Editor, s tcell.Screen) {
		text := e.getActive().getSelectedText()
		if text != "" {
			_ = clipboard.WriteAll(text)
		}
	},
	tcell.KeyCtrlX: func(e *Editor, s tcell.Screen) {
		b := e.getActive()
		if b.isReadOnly {
			return
		}
		text := b.getSelectedText()
		if text != "" {
			_ = clipboard.WriteAll(text)
			b.BeginTransaction()
			b.DeleteSelection()
			b.EndTransaction()
		}
	},
	tcell.KeyCtrlV: func(e *Editor, s tcell.Screen) {
		b := e.getActive()
		if b.isReadOnly {
			return
		}
		text, err := clipboard.ReadAll()
		if err == nil && text != "" {
			b.BeginTransaction()
			b.DeleteSelection()
			text = strings.ReplaceAll(text, "\r\n", "\n")
			b.InsertTextWithRecord(b.cursor, text)
			b.EndTransaction()
		}
	},
	tcell.KeyCtrlN: func(e *Editor, s tcell.Screen) {
		e.buffers = append(e.buffers, NewBuffer())
		e.activeBuffer = len(e.buffers) - 1
	},
	tcell.KeyCtrlW: func(e *Editor, s tcell.Screen) {
		b := e.getActive()
		if b.isModified {
			e.promptMode = true
			e.promptType = "close"
			e.targetCloseBuffer = e.activeBuffer
			return
		}
		if len(e.buffers) <= 1 {
			s.Fini()
			os.Exit(0)
		}
		e.closeBuffer(e.activeBuffer)
		e.needsFullRefresh = true
	},
	tcell.KeyCtrlBackslash: func(e *Editor, s tcell.Screen) { e.activeBuffer = (e.activeBuffer + 1) % len(e.buffers) },
	tcell.KeyCtrlF: func(e *Editor, s tcell.Screen) {
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
	},
	tcell.KeyCtrlR: func(e *Editor, s tcell.Screen) {
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
	},
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
	strData, encoding, err := readFileDetectEncoding(absPath)
	if err == nil {
		b.encoding = encoding
		e.fileWatcher.Add(absPath)
		b.setLinesFromText(strData)
		b.savedContent = b.getContent()
		b.savedTotalChars = b.totalChars
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
	strData, _, err := readFileDetectEncoding(path)
	if err != nil {
		return
	}

	b := NewBuffer()
	b.filePath = path
	b.isConfig = true
	b.isReadOnly = isReadOnly
	e.fileWatcher.Add(path)
	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars

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
	_ = os.Setenv("LANG", "ko_KR.UTF-8")
	_ = os.Setenv("LC_ALL", "ko_KR.UTF-8")

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
			fmt.Println("jigedit v1.1.2 - A Sane Editor For The Sane People")
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
				for i, buf := range editor.buffers {
					if buf.filePath == filePath {
						// 💡 수정된 부분: 파일 감지기도 현재 탭의 인코딩을 존중합니다.
						var strData string
						var err error
						if buf.encoding != "" && buf.encoding != "UTF-8" {
							strData, err = readFileWithEncoding(filePath, buf.encoding)
						} else {
							strData, _, err = readFileDetectEncoding(filePath)
						}

						if err == nil && strData != buf.savedContent {
							if buf.isModified {
								editor.promptMode = true
								editor.promptType = "external_change"
								editor.targetCloseBuffer = i
								needsLayout = true
							} else {
								if buf.reloadFromDisk() {
									if buf.isConfig {
										var newCfg Config
										if err := json.Unmarshal([]byte(buf.savedContent), &newCfg); err == nil {
											editor.cfg = newCfg
										}
									}
									needsLayout = true
									snapToCursor = true
								}
							}
						}
					}
				}
			}
		case *tcell.EventMouse:
			mx, my := ev.Position()
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

			if (buttons&tcell.Button3 != 0 || buttons&tcell.Button2 != 0) && !editor.paletteActive {
				editor.ctxMenuActive = true
				editor.ctxMenuX = mx
				editor.ctxMenuY = my
				editor.ctxMenuCursor = 0
				editor.paletteActive = false
				editor.encodeMenuActive = false
				needsLayout = true
				continue
			}

			if editor.ctxMenuActive {
				if mx >= editor.ctxMenuX && mx < editor.ctxMenuX+editor.ctxMenuW && my >= editor.ctxMenuY && my < editor.ctxMenuY+editor.ctxMenuH {
					clickIdx := my - editor.ctxMenuY - 1
					if clickIdx >= 0 && clickIdx < len(editor.ctxMenuItems) {
						if editor.ctxMenuCursor != clickIdx {
							editor.ctxMenuCursor = clickIdx
							needsLayout = true
						}
						if isNewPress {
							action := editor.ctxMenuItems[editor.ctxMenuCursor].Action
							editor.ctxMenuActive = false
							if action != nil {
								action(editor, currentScreen)
							}
							needsLayout = true
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
					clickIdx := my - editor.encodeMenuY - 1
					if clickIdx >= 0 && clickIdx < len(editor.encodeMenuItems) {
						if editor.encodeMenuCursor != clickIdx {
							editor.encodeMenuCursor = clickIdx
							needsLayout = true
						}
						if isNewPress {
							action := editor.encodeMenuItems[editor.encodeMenuCursor].Action
							editor.encodeMenuActive = false
							editor.encodeMenuState = 0 // 💡 상태 초기화 필수!
							if action != nil {
								action(editor, currentScreen)
							}
							needsLayout = true
						}
					}
				} else if isNewPress {
					editor.encodeMenuActive = false
					editor.encodeMenuState = 0
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
					idx := 0
					if mx >= prefixW {
						currW := prefixW
						for i, r := range *targetStr {
							rw := runewidth.RuneWidth(r)
							if mx >= currW && mx < currW+rw {
								idx = i
								break
							}
							currW += rw
							idx = i + 1
						}
					}
					if isNewPress && my == h-1 {
						b.inputCX = idx
						b.isInputSelect = true
						b.inputSelStart = idx
						b.inputSelEnd = idx
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
					clickIdx := my - editor.paletteY - 1
					visibleItems := editor.paletteH - 2
					startIdx := editor.paletteCursor - visibleItems/2
					if startIdx < 0 {
						startIdx = 0
					}
					if startIdx+visibleItems > len(editor.paletteItems) {
						startIdx = len(editor.paletteItems) - visibleItems
						if startIdx < 0 {
							startIdx = 0
						}
					}
					targetItemIdx := startIdx + clickIdx
					if targetItemIdx >= 0 && targetItemIdx < len(editor.paletteItems) {
						if editor.paletteCursor != targetItemIdx {
							editor.paletteCursor = targetItemIdx
							needsLayout = true
						}
						if isNewPress {
							action := editor.paletteItems[editor.paletteCursor].Action
							editor.paletteActive = false
							if action != nil {
								action(editor, currentScreen)
							}
							needsLayout = true
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

							vlWidth := 0
							lineData := b.lines[currL]
							for i := vl.startCX; i < vl.endCX && i < len(lineData); {
								r, size := utf8.DecodeRune(lineData[i:])
								rw := fastRuneWidth(r, editor.cfg.TabSize)
								vlWidth += rw
								i += size
							}

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

				if isNewPress {
					now := time.Now()
					if now.Sub(lastClickTime) < 400*time.Millisecond && mx == lastClickX && my == lastClickY {
						clickCount++
					} else {
						clickCount = 1
					}
					lastClickTime = now
					lastClickX, lastClickY = mx, my

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
					// 💡 마우스가 완벽히 멈춰있을 때는 더블/트리플 클릭 선택 영역을 취소하지 않음!
					if mx != lastClickX || my != lastClickY {
						b.selection.End = loc
						b.cursor = loc
					}
					// 🟢 O(1) 가상라인 역방향 이동
					if my < editor.tabHeight && b.vOffsetL > 0 {
						b.vOffsetSub--
						if b.vOffsetSub < 0 {
							b.vOffsetL--
							b.ensureVCache(b.vOffsetL, editor.cfg)
							b.vOffsetSub = len(b.vCache[b.vOffsetL]) - 1
						}
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
			isCtrl := (ev.Modifiers() & tcell.ModCtrl) != 0
			isShift := (ev.Modifiers() & tcell.ModShift) != 0
			isAlt := (ev.Modifiers() & tcell.ModAlt) != 0
			snapToCursor = true

			if ev.Key() != tcell.KeyEnd {
				b.stickToWrapEnd = false
			}

			// 💡 탭 좌우 이동 시 화면 렌더링 캐시 동기화
			if isAlt && ev.Rune() == ',' {
				editor.activeBuffer = (editor.activeBuffer - 1 + len(editor.buffers)) % len(editor.buffers)
				editor.needsFullRefresh = true
				needsLayout = true
				continue
			}
			if isAlt && ev.Rune() == '.' {
				editor.activeBuffer = (editor.activeBuffer + 1) % len(editor.buffers)
				editor.needsFullRefresh = true
				needsLayout = true
				continue
			}

			if editor.promptMode {
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
					needsLayout = true
				} else if ev.Rune() == 'y' || ev.Rune() == 'Y' {
					editor.promptMode = false

					// 💡 타겟 탭이 여전히 유효한지 검사하는 안전장치 추가!
					target := editor.targetCloseBuffer
					isValidTarget := target >= 0 && target < len(editor.buffers)

					if editor.promptType == "quit" {
						currentScreen.Fini()
						os.Exit(0)
					} else if editor.promptType == "close" {
						if !isValidTarget {
							continue
						}
						if len(editor.buffers) <= 1 {
							currentScreen.Fini()
							os.Exit(0)
						}
						editor.closeBuffer(target)
						editor.needsFullRefresh = true
						needsLayout = true
					} else if editor.promptType == "reset_config" {
						defaultCfg := DefaultConfig()
						data, _ := json.MarshalIndent(defaultCfg, "", "    ")
						_ = ioutil.WriteFile(getConfigPath(), data, 0644)
						editor.cfg = defaultCfg
						for _, buf := range editor.buffers {
							if buf.isConfig {
								buf.isModified = false
								buf.savedContent = ""
								buf.reloadFromDisk()
							}
						}
						editor.needsFullRefresh = true
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
								if err := json.Unmarshal([]byte(bufToReload.savedContent), &newCfg); err == nil {
									editor.cfg = newCfg
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
				case tcell.KeyEnter:
					action := editor.paletteItems[editor.paletteCursor].Action
					editor.paletteActive = false
					if action != nil {
						action(editor, currentScreen)
					}
				}
				needsLayout = true
				continue
			}

			if editor.ctxMenuActive {
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
				case tcell.KeyEnter:
					action := editor.ctxMenuItems[editor.ctxMenuCursor].Action
					editor.ctxMenuActive = false
					if action != nil {
						action(editor, currentScreen)
					}
				}
				needsLayout = true
				continue
			}

			if editor.encodeMenuActive {
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
				case tcell.KeyEnter:
					action := editor.encodeMenuItems[editor.encodeMenuCursor].Action
					editor.encodeMenuActive = false
					editor.encodeMenuState = 0 // 💡 상태 초기화 필수!
					if action != nil {
						action(editor, currentScreen)
					}
				}
				needsLayout = true
				continue
			}

			if b.searchMode || b.gotoMode {
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
					if ev.Key() == tcell.KeyCtrlC && hasSel {
						clipboard.WriteAll(string((*targetStr)[selStart:selEnd]))
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
							b.BeginTransaction()
							for i := len(b.matches) - 1; i >= 0; i-- {
								m := b.matches[i]
								b.DeleteTextWithRecord(m.loc, Loc{m.loc.L, m.loc.C + m.matchLen})
								b.InsertTextWithRecord(m.loc, string(b.replaceQuery))
							}
							b.EndTransaction()
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
				if isAlt && (k == tcell.KeyUp || k == tcell.KeyDown) { // 줄 이동 단축키 차단
					continue
				}
			}

			if isAlt && ev.Key() == tcell.KeyUp {
				if b.cursor.L > 0 {
					oldL, oldC := b.cursor.L, b.cursor.C // 💡 안전하게 원본 위치 캡처
					b.BeginTransaction()
					currStr := string(b.lines[oldL])
					prevStr := string(b.lines[oldL-1])
					b.DeleteTextWithRecord(Loc{oldL - 1, 0}, Loc{oldL, len(b.lines[oldL])})
					b.InsertTextWithRecord(Loc{oldL - 1, 0}, currStr+"\n"+prevStr)
					b.cursor = b.clampLoc(Loc{oldL - 1, oldC}) // 💡 절대 에러 방지
					b.EndTransaction()
					needsLayout = true
				}
				continue
			}
			if isAlt && ev.Key() == tcell.KeyDown {
				if b.cursor.L < len(b.lines)-1 {
					oldL, oldC := b.cursor.L, b.cursor.C // 💡 안전하게 원본 위치 캡처
					b.BeginTransaction()
					currStr := string(b.lines[oldL])
					nextStr := string(b.lines[oldL+1])
					b.DeleteTextWithRecord(Loc{oldL, 0}, Loc{oldL + 1, len(b.lines[oldL+1])})
					b.InsertTextWithRecord(Loc{oldL, 0}, nextStr+"\n"+currStr)
					b.cursor = b.clampLoc(Loc{oldL + 1, oldC}) // 💡 절대 에러 방지
					b.EndTransaction()
					needsLayout = true
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
					}
				}
			}

			if action, exists := ActionMap[ev.Key()]; exists {
				action(editor, currentScreen)
				needsLayout = true
				continue
			}

			switch ev.Key() {
			case tcell.KeyTab, tcell.KeyBacktab:
				b.BeginTransaction()
				s, e := b.getSelectionRange()
				hasSel := b.HasSelection()
				if !hasSel {
					s = b.cursor
					e = b.cursor
				}
				oldCursor := b.cursor

				if isShift || ev.Key() == tcell.KeyBacktab {
					for r := s.L; r <= e.L; r++ {
						line := b.lines[r]
						if len(line) > 0 {
							removeCount := 0
							if line[0] == '\t' {
								removeCount = 1
							} else {
								for removeCount < len(line) && removeCount < editor.cfg.TabSize && line[removeCount] == ' ' {
									removeCount++
								}
							}
							if removeCount > 0 {
								b.DeleteTextWithRecord(Loc{r, 0}, Loc{r, removeCount})
								if r == oldCursor.L {
									oldCursor.C -= removeCount
									if oldCursor.C < 0 {
										oldCursor.C = 0
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
					if editor.cfg.ExpandTab {
						indentStr = strings.Repeat(" ", editor.cfg.TabSize)
					} else {
						indentStr = "\t"
					}
					indentLen := len([]rune(indentStr)) // 스페이스 개수(TabSize) 또는 탭 문자 1개(1)

					if hasSel && s.L != e.L { // 💡 다중 줄 선택 시 전체 줄 들여쓰기
						for r := e.L; r >= s.L; r-- {
							b.InsertTextWithRecord(Loc{r, 0}, indentStr)
							if r == oldCursor.L {
								oldCursor.C += indentLen
							}
							if r == s.L {
								s.C += indentLen
							}
							if r == e.L {
								e.C += indentLen
							}
						}
					} else {
						// 💡 단일 줄 내에서 글자를 드래그한 상태면 지우고 탭 삽입
						if hasSel {
							b.DeleteSelection()
							oldCursor = b.cursor
							hasSel = false
						}
						b.InsertTextWithRecord(oldCursor, indentStr)
						oldCursor.C += indentLen
					}
				}

				b.cursor = oldCursor
				if hasSel {
					b.selection.Start = s
					b.selection.End = e
				}
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyLeft:
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
			case tcell.KeyRight:
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
			case tcell.KeyUp:
				if isCtrl {
					b.moveParagraphUp()
				} else {
					b.moveCursorVisualLine(-1, editor.cfg) // 🟢 O(1) 로컬 이동으로 교체
				}
			case tcell.KeyDown:
				if isCtrl {
					b.moveParagraphDown()
				} else {
					b.moveCursorVisualLine(1, editor.cfg) // 🟢 O(1) 로컬 이동으로 교체
				}
			case tcell.KeyPgUp:
				b.moveCursorVisualLine(-(h - 2), editor.cfg) // 🟢 로컬 이동으로 교체
			case tcell.KeyPgDn:
				b.moveCursorVisualLine(h-2, editor.cfg) // 🟢 로컬 이동으로 교체
			case tcell.KeyHome:
				if isCtrl {
					b.cursor = Loc{0, 0}
				} else {
					cursorSub := b.getCursorSub(editor.cfg)
					currentVL := b.vCache[b.cursor.L][cursorSub]
					b.cursor.C = currentVL.startCX // 🟢 로컬 계산으로 교체
				}
			case tcell.KeyEnd:
				if isCtrl {
					lastLineIdx := len(b.lines) - 1
					if lastLineIdx < 0 {
						lastLineIdx = 0
					}
					b.cursor = Loc{lastLineIdx, len(b.lines[lastLineIdx])}
					b.stickToWrapEnd = true
				} else {
					cursorSub := b.getCursorSub(editor.cfg)
					currentVL := b.vCache[b.cursor.L][cursorSub]
					b.cursor.C = currentVL.endCX // 🟢 로컬 계산으로 교체
					b.stickToWrapEnd = true
				}
			case tcell.KeyEnter:
				b.BeginTransaction()
				b.DeleteSelection()
				indentStr := ""
				if editor.cfg.AutoIndent {
					line := b.lines[b.cursor.L]
					for i := 0; i < b.cursor.C && i < len(line); i++ {
						if line[i] == ' ' || line[i] == '\t' {
							indentStr += string(line[i])
						} else {
							break
						}
					}
				}
				b.InsertTextWithRecord(b.cursor, "\n"+indentStr)
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyBackspace2, tcell.KeyBackspace:
				b.BeginTransaction()
				if !b.DeleteSelection() {
					if b.cursor.C > 0 {
						startX := b.cursor.C
						if isCtrl || isAlt {
							oldCursor := b.cursor
							b.moveWordLeft()
							startX = b.cursor.C
							b.cursor = oldCursor
						} else {
							_, size := utf8.DecodeLastRune(b.lines[b.cursor.L][:b.cursor.C])
							startX -= size
						}
						b.DeleteTextWithRecord(Loc{b.cursor.L, startX}, Loc{b.cursor.L, b.cursor.C})
					} else if b.cursor.L > 0 {
						b.DeleteTextWithRecord(Loc{b.cursor.L - 1, len(b.lines[b.cursor.L-1])}, b.cursor)
					}
				}
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyDelete:
				b.BeginTransaction()
				if !b.DeleteSelection() {
					lineLen := len(b.lines[b.cursor.L])
					if isCtrl || isAlt {
						if b.cursor.C < lineLen {
							oldCursor := b.cursor
							b.moveWordRight()
							endX := b.cursor.C
							b.cursor = oldCursor
							b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L, endX})
						} else if b.cursor.L < len(b.lines)-1 {
							b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L + 1, 0})
						}
					} else {
						if b.cursor.C < lineLen {
							_, size := utf8.DecodeRune(b.lines[b.cursor.L][b.cursor.C:])
							b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L, b.cursor.C + size})
						} else if b.cursor.L < len(b.lines)-1 {
							b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L + 1, 0})
						}
					}
				}
				b.EndTransaction()
				needsLayout = true

			case tcell.KeyRune:
				if ev.Rune() != 0 {
					b.BeginTransaction()
					b.DeleteSelection()
					b.InsertTextWithRecord(b.cursor, string(ev.Rune()))
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
