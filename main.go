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
	"sync"
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
	LineWrappingCap int    `json:"line_wrapping_cap"`
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
		LineWrappingCap: 20000,
	}
}

func getConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "myeditor", "config.json")
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




func convertLinuxDateToGoLayout(linuxFormat string) string {
	replacer := strings.NewReplacer(
		"%Y", "2006", "%y", "06", "%m", "01", "%d", "02",
		"%H", "15", "%I", "03", "%M", "04", "%S", "05",
		"%p", "PM", "%a", "Mon", "%A", "Monday", "%b", "Jan", "%B", "January",
	)
	return replacer.Replace(linuxFormat)
}

// 💡 이름으로 인코딩 객체를 매핑해주는 범용 엔진
func getTextEncoding(name string) encoding.Encoding {
	switch name {
		case "EUC-KR":
			return korean.EUCKR
		case "Shift-JIS":
			return japanese.ShiftJIS
		case "EUC-JP":
			return japanese.EUCJP
		case "GBK":
			return simplifiedchinese.GBK
		case "CP1252":
			return charmap.Windows1252
		default:
			return nil // UTF-8 또는 지원하지 않는 경우
	}
}

// 💡 파일을 처음 열 때 자동 추론하는 함수 (기존 로직 유지)
// 💡 파일을 처음 열 때 다국어 인코딩을 자동 추론하는 엔진
func readFileDetectEncoding(path string) (string, string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	
	// 1. 순수 UTF-8 검증 (가장 빠르고 확실함)
	if utf8.Valid(data) {
		return string(data), "UTF-8", nil
	}

	// 2. 다른 인코딩 후보들 테스트 (우선순위: 한국어 -> 일본어 -> 중국어 -> 서유럽)
	candidates := []string{"EUC-KR", "Shift-JIS", "EUC-JP", "GBK", "CP1252"}
	
	var bestDecoded []byte
	bestEnc := "UTF-8"
	lowestErrors := len(data) + 1 // 최소 에러 개수 갱신용 (초기값은 무한대 대용)

	for _, encName := range candidates {
		enc := getTextEncoding(encName)
		if enc == nil {
			continue
		}

		// 해당 인코딩으로 변환 시도
		reader := transform.NewReader(bytes.NewReader(data), enc.NewDecoder())
		decoded, err := ioutil.ReadAll(reader)
		if err != nil {
			continue
		}

		// 변환된 결과물에 디코딩 실패/깨진 글자(Replacement Character, \uFFFD)가 몇 개인지 카운트
		errorCount := bytes.Count(decoded, []byte("\uFFFD"))
		
		// 깨진 글자가 단 하나도 없다면, 이 인코딩이 거의 100% 확실하므로 즉시 반환
		if errorCount == 0 {
			return string(decoded), encName, nil
		}

		// 만약 완벽한 인코딩이 없다면, 가장 깨진 글자가 적은(오류가 적은) 인코딩을 기억해둠
		if errorCount < lowestErrors {
			lowestErrors = errorCount
			bestDecoded = decoded
			bestEnc = encName
		}
	}

	// 3. 후보를 다 돌았는데도 완벽한 걸 못 찾았으면 차선책 반환
	if bestDecoded != nil {
		return string(bestDecoded), bestEnc, nil
	}

	// 최악의 경우 원본 반환
	return string(data), "UTF-8", nil
}

// 💡 사용자가 강제로 인코딩을 지정해서 다시 읽어오는 함수 (Reopen)
func readFileWithEncoding(path string, encName string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
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
		return ioutil.WriteFile(path, []byte(content), 0644)
	}
	var buf bytes.Buffer
	writer := transform.NewWriter(&buf, enc.NewEncoder())
	writer.Write([]byte(content))
	writer.Close()
	return ioutil.WriteFile(path, buf.Bytes(), 0644)
}



// --- [ 2. 자료구조 정의 ] ---

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
	lineIdx   int
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
	lines        [][]rune 
	vCache       [][]VisualLine // 💡 핵심: 각 줄마다 줄바꿈 상태를 기억하는 영구 캐시 배열!
	cursor       Loc      
	selection    Range
	isSelecting  bool

	vOffsetIdx   int
	hOffset      int
	isReadOnly   bool

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

	undoStack []Transaction
	redoStack []Transaction
	currentTx *Transaction
	txIDCounter  int64
	savedTxID    int64

	gotoMode     bool
	gotoInput    []rune
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
	cachedVLines    []VisualLine
	vLinesValid     bool
	cachedMaxWidth  int
	cachedConfig    Config
	totalVLines     int
	totalChars      int
	
	// Net Change 감지용
	savedTotalChars int
	syncTimer       *time.Timer
	contentMutex    sync.Mutex
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

	// 💡 추가됨: 인코딩 선택 메뉴 상태
	encodeMenuActive bool
	encodeMenuItems  []PaletteItem
	encodeMenuCursor int
	encodeMenuX      int
	encodeMenuY      int
	encodeMenuW      int
	encodeMenuH      int

	needsFullRefresh bool
	prevW            int
	prevH            int
	screenCache      [][]cellState

	prevActiveBuf int
	prevVOffset   int
	prevHOffset   int
	prevPalette   bool
	prevCtxMenu   bool
	prevEncode    bool
	prevPrompt    bool
	prevSearch    bool
	prevGoto      bool
	prevReplace   bool
	prevLinesLen  int
	nextCache     [][]cellState // 💡 최적화: 화면 버퍼 재사용을 위한 캐시 변수 추가
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
				if !ok { return }
				if event.Op&fsnotify.Write == fsnotify.Write {
					for _, buf := range e.buffers {
						if buf.filePath == event.Name {
							if globalScreenHandle != nil && *globalScreenHandle != nil {
								(*globalScreenHandle).PostEvent(tcell.NewEventInterrupt(buf.filePath))
							}
						}
					}
				}
			case err, ok := <-e.fileWatcher.Errors:
				if !ok { return }
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


func (e *Editor) initEncodingMenu() {
	encodings := []string{"UTF-8", "EUC-KR", "Shift-JIS", "EUC-JP", "GBK", "CP1252"}
	e.encodeMenuItems = []PaletteItem{}

	for _, enc := range encodings {
		encName := enc
		e.encodeMenuItems = append(e.encodeMenuItems, PaletteItem{
			Name:     fmt.Sprintf("저장 인코딩 변경 (Save As): %s", encName),
					   Shortcut: "",
					   Action: func(e *Editor, s tcell.Screen) {
						   b := e.getActive()
						   if b.encoding != encName {
							   b.encoding = encName
							   b.isModified = true
						   }
					   },
		})
	}

	e.encodeMenuItems = append(e.encodeMenuItems, PaletteItem{Name: "------------------------------------", Shortcut: "", Action: nil})

	for _, enc := range encodings {
		encName := enc
		e.encodeMenuItems = append(e.encodeMenuItems, PaletteItem{
			Name:     fmt.Sprintf("다시 열기 (Reopen with): %s", encName),
					   Shortcut: "",
					   Action: func(e *Editor, s tcell.Screen) {
						   b := e.getActive()
						   if b.filePath == "" { return }

						   // 💡 사용자가 텍스트를 수정 중이라면 경고창(Prompt) 띄우기!
						   if b.isModified {
							   e.promptMode = true
							   e.promptType = "reopen"
							   e.targetEncoding = encName
							   return
						   }

						   // 수정한 게 없으면 바로 다시 열기 실행
						   b.reopenWithEncoding(encName)
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
	if loc.L < 0 { loc.L = 0 }
	if loc.L >= len(b.lines) { loc.L = len(b.lines) - 1 }
	if loc.C < 0 { loc.C = 0 }
	if loc.C > len(b.lines[loc.L]) { loc.C = len(b.lines[loc.L]) }
	return loc
}

// 💡 텍스트를 파일이나 문자열에서 읽어와 순수 2차원 배열로 세팅
func (b *Buffer) setLinesFromText(strData string) {
	var newLines [][]rune
	currentLine := []rune{}
	for _, r := range strData {
		if r == '\n' {
			newLines = append(newLines, currentLine)
			currentLine = []rune{}
		} else if r != '\r' {
			currentLine = append(currentLine, r)
		}
	}
	newLines = append(newLines, currentLine)
	b.lines = newLines
	b.vCache = make([][]VisualLine, len(newLines)) // 💡 초기화
	b.vLinesValid = false
	b.totalChars = utf8.RuneCountInString(strData)
}

func (b *Buffer) getContent() string {
	var sb strings.Builder
	sb.Grow(b.totalChars + len(b.lines))
	for i, line := range b.lines {
		sb.WriteString(string(line))
		if i < len(b.lines)-1 { sb.WriteByte('\n') }
	}
	return sb.String()
}

func NewBuffer() *Buffer {
	b := &Buffer{
		lines:    [][]rune{{}},
		vCache:   [][]VisualLine{nil}, // 💡 초기화
		cursor:   Loc{L: 0, C: 0},
		encoding: "UTF-8",
	}
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars
	return b
}


// 💡 메모리를 단 1바이트도 쓰지 않는(Zero-Allocation) 극한의 원본 비교 엔진
func (b *Buffer) isContentEqual() bool {
	// 회원님 아이디어: 글자 수 다르면 무조건 다른 거니까 0.0001초 컷
	if b.totalChars != b.savedTotalChars {
		return false
	}

	lineIdx, colIdx := 0, 0
	// 2차원 배열을 하나로 합치지 않고, 원본 문자열을 순회하며 실시간 좌표 비교!
	for _, r := range b.savedContent {
		if r == '\n' {
			if lineIdx >= len(b.lines) || colIdx != len(b.lines[lineIdx]) {
				return false
			}
			lineIdx++
			colIdx = 0
			continue
		}
		
		if lineIdx >= len(b.lines) || colIdx >= len(b.lines[lineIdx]) || b.lines[lineIdx][colIdx] != r {
			return false
		}
		colIdx++
	}
	return lineIdx == len(b.lines)-1 && colIdx == len(b.lines[lineIdx])
}

func (b *Buffer) checkModified() { 
	var currentTxID int64 = 0
	if len(b.undoStack) > 0 {
		currentTxID = b.undoStack[len(b.undoStack)-1].ID
	}
	
	if currentTxID == b.savedTxID {
		b.isModified = false
		return
	}

	// 💡 회원님 아이디어 적용: 길이가 다르면 더 볼 것도 없이 무조건 수정된 것!
	if b.totalChars != b.savedTotalChars {
		b.isModified = true
		return
	}

	b.isModified = true 

	if b.syncTimer != nil {
		b.syncTimer.Stop()
	}
	
	// 글자수가 우연히 똑같을 때만 100ms초 뒤에 백그라운드 검사
	b.syncTimer = time.AfterFunc(100*time.Millisecond, func() {
		b.contentMutex.Lock()
		defer b.contentMutex.Unlock()
		
		// 💡 무거운 getContent() 대신 메모리 점유율 0%인 극한 비교기 사용
		if b.isContentEqual() {
			b.isModified = false
			if globalScreenHandle != nil && *globalScreenHandle != nil {
				(*globalScreenHandle).PostEvent(tcell.NewEventInterrupt("refresh_mod_state"))
			}
		}
	})
}

func (b *Buffer) markSaved() {
	if len(b.undoStack) == 0 {
		b.savedTxID = 0
	} else {
		b.savedTxID = b.undoStack[len(b.undoStack)-1].ID
	}
	b.savedTotalChars = b.totalChars // 💡 저장 당시 글자 수 기록
	b.isModified = false
}


func (b *Buffer) reloadFromDisk() bool {
	if b.filePath == "" || b.isModified { return false }
	strData, _, err := readFileDetectEncoding(b.filePath)
	if err != nil || strData == b.savedContent { return false }

	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars // 💡 글자 수 완벽 동기화 (누락 방지)
	b.lastExternalSync = time.Now()
	b.cursor = b.clampLoc(b.cursor)
	b.undoStack = nil; b.redoStack = nil; b.isSelecting = false
	b.txIDCounter = 0; b.savedTxID = 0
	return true
}

func (b *Buffer) reopenWithEncoding(encName string) {
	strData, err := readFileWithEncoding(b.filePath, encName)
	if err != nil { return }
	
	b.encoding = encName
	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars // 💡 글자 수 완벽 동기화 (누락 방지)
	b.isModified = false
	b.lastExternalSync = time.Now()
	b.cursor = Loc{0, 0}; b.vOffsetIdx = 0; b.hOffset = 0
	b.undoStack = nil; b.redoStack = nil; b.isSelecting = false
	b.txIDCounter = 0; b.savedTxID = 0
}

func (b *Buffer) Insert(loc Loc, text string) Loc {
	loc = b.clampLoc(loc)
	runes := []rune(text)
	if len(runes) == 0 { return loc }
	b.totalChars += len(runes)

	var newLines [][]rune
	start := 0
	for i, r := range runes {
		if r == '\n' {
			newLines = append(newLines, append([]rune(nil), runes[start:i]...))
			start = i + 1
		}
	}
	newLines = append(newLines, append([]rune(nil), runes[start:]...))
	newCache := make([][]VisualLine, len(newLines))

	if len(newLines) == 1 {
		line := b.lines[loc.L]
		newLine := make([]rune, 0, len(line)+len(newLines[0]))
		newLine = append(newLine, line[:loc.C]...); newLine = append(newLine, newLines[0]...); newLine = append(newLine, line[loc.C:]...)
		b.lines[loc.L] = newLine
		b.vCache[loc.L] = nil // 💡 수정한 줄의 캐시 무효화
		return Loc{L: loc.L, C: loc.C + len(newLines[0])}
	}

	originalLine := b.lines[loc.L]
	tail := append([]rune(nil), originalLine[loc.C:]...)
	firstLine := make([]rune, 0, loc.C+len(newLines[0]))
	firstLine = append(firstLine, originalLine[:loc.C]...); firstLine = append(firstLine, newLines[0]...)
	
	b.lines[loc.L] = firstLine
	b.vCache[loc.L] = nil // 💡 캐시 무효화
	newLines[len(newLines)-1] = append(newLines[len(newLines)-1], tail...)

	// 💡 최적화: O(N) 임시 슬라이스 할당(Double Copy)을 방지하는 In-place Shift
	addLen := len(newLines) - 1
	b.lines = append(b.lines, make([][]rune, addLen)...)
	b.vCache = append(b.vCache, make([][]VisualLine, addLen)...)

	copy(b.lines[loc.L+addLen+1:], b.lines[loc.L+1:])
	copy(b.vCache[loc.L+addLen+1:], b.vCache[loc.L+1:])

	copy(b.lines[loc.L+1:], newLines[1:])
	copy(b.vCache[loc.L+1:], newCache[1:])

	return Loc{L: loc.L + len(newLines) - 1, C: len(newLines[len(newLines)-1]) - len(tail)}
}


func (b *Buffer) Remove(start, end Loc) string {
	start = b.clampLoc(start); end = b.clampLoc(end)
	if start.L > end.L || (start.L == end.L && start.C > end.C) { start, end = end, start }
	if start.L == end.L && start.C == end.C { return "" }

	if start.L == end.L {
		line := b.lines[start.L]
		deleted := string(line[start.C:end.C])
		newLine := make([]rune, 0, len(line)-(end.C-start.C))
		newLine = append(newLine, line[:start.C]...); newLine = append(newLine, line[end.C:]...)
		b.lines[start.L] = newLine
		b.vCache[start.L] = nil // 💡 캐시 무효화
		b.totalChars -= len([]rune(deleted))
		return deleted
	}

	var deletedRunes []rune
	lineStart := b.lines[start.L]; lineEnd := b.lines[end.L]
	
	// 💡 최적화: 용량을 미리 계산하여 삭제 시 불필요한 배열 재할당 방지
	delCap := len(lineStart) - start.C + 1
	for i := start.L + 1; i < end.L; i++ { delCap += len(b.lines[i]) + 1 }
	delCap += end.C
	deletedRunes = make([]rune, 0, delCap)
	
	deletedRunes = append(deletedRunes, lineStart[start.C:]...); deletedRunes = append(deletedRunes, '\n')
	for i := start.L + 1; i < end.L; i++ { deletedRunes = append(deletedRunes, b.lines[i]...); deletedRunes = append(deletedRunes, '\n') }
	deletedRunes = append(deletedRunes, lineEnd[:end.C]...)

	newLine := make([]rune, 0, start.C+len(lineEnd)-end.C)
newLine = append(newLine, lineStart[:start.C]...); newLine = append(newLine, lineEnd[end.C:]...)
	b.lines[start.L] = newLine
	b.vCache[start.L] = nil // 💡 캐시 무효화

	// 💡 최적화: 임시 배열 생성 없이 제자리에서 잘라내기 병합 (단일 복사)
	oldLen := len(b.lines)
	b.lines = append(b.lines[:start.L+1], b.lines[end.L+1:]...)
	b.vCache = append(b.vCache[:start.L+1], b.vCache[end.L+1:]...)

	// 💡 메모리 누수 방지: 슬라이스 축소 후 꼬리 부분에 남은 포인터들을 nil로 초기화 (GC 수거 지원)
	for i := len(b.lines); i < oldLen; i++ {
		b.lines[:cap(b.lines)][i] = nil
		b.vCache[:cap(b.vCache)][i] = nil
	}

	deletedStr := string(deletedRunes)
	b.totalChars -= len([]rune(deletedStr))
	return deletedStr
}
// --- [ 4. 코어 논리 엔진 (Undo/Redo, Selection, Movement, VisualLine) ] ---

// 💡 1. 절대 꼬이지 않는 초간단 Undo / Redo 시스템
func (b *Buffer) BeginTransaction() {
	if b.currentTx == nil { b.currentTx = &Transaction{BeforeLoc: b.cursor} }
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
			newStack := make([]Transaction, 800)
			copy(newStack, b.undoStack[201:])
			b.undoStack = newStack
		}
		b.checkModified()
	}
	b.currentTx = nil
}
func (b *Buffer) InsertTextWithRecord(loc Loc, text string) {
	if b.isReadOnly || text == "" { return }
	endLoc := b.Insert(loc, text)
	if b.currentTx != nil {
		b.currentTx.Actions = append(b.currentTx.Actions, Action{IsInsert: true, Start: loc, End: endLoc, Text: text})
	}
	b.cursor = endLoc
}
func (b *Buffer) DeleteTextWithRecord(start, end Loc) {
	if b.isReadOnly { return }
	text := b.Remove(start, end)
	if text != "" && b.currentTx != nil {
		b.currentTx.Actions = append(b.currentTx.Actions, Action{IsInsert: false, Start: start, End: end, Text: text})
	}
	b.cursor = start
}
func (b *Buffer) DeleteSelection() bool {
	if b.isReadOnly || !b.HasSelection() { return false }
	start, end := b.getSelectionRange()
	b.DeleteTextWithRecord(start, end)
	b.clearSelection()
	return true
}
func (b *Buffer) Undo() {
	if b.isReadOnly || len(b.undoStack) == 0 { return }
	tx := b.undoStack[len(b.undoStack)-1]
	b.undoStack = b.undoStack[:len(b.undoStack)-1]
	for i := len(tx.Actions) - 1; i >= 0; i-- {
		act := tx.Actions[i]
		if act.IsInsert { b.Remove(act.Start, act.End) } else { b.Insert(act.Start, act.Text) }
	}
	b.cursor = b.clampLoc(tx.BeforeLoc)
	b.redoStack = append(b.redoStack, tx)
	b.checkModified(); b.clearSelection()
}
func (b *Buffer) Redo() {
	if b.isReadOnly || len(b.redoStack) == 0 { return }
	tx := b.redoStack[len(b.redoStack)-1]
	b.redoStack = b.redoStack[:len(b.redoStack)-1]
	for _, act := range tx.Actions {
		if act.IsInsert { b.Insert(act.Start, act.Text) } else { b.Remove(act.Start, act.End) }
	}
	b.cursor = b.clampLoc(tx.AfterLoc)
	b.undoStack = append(b.undoStack, tx)
	b.checkModified(); b.clearSelection()
}

// 💡 2. 절대 좌표 기반 선택(Selection) 조작 로직
func (b *Buffer) getSelectionRange() (Loc, Loc) {
	s, e := b.selection.Start, b.selection.End
	if s.L > e.L || (s.L == e.L && s.C > e.C) { return e, s }
	return s, e
}
func (b *Buffer) HasSelection() bool { return b.selection.Start != b.selection.End }
func (b *Buffer) clearSelection() { b.isSelecting = false; b.selection.Start = b.cursor; b.selection.End = b.cursor }
func (b *Buffer) selectAll() {
	b.isSelecting = true
	b.selection.Start = Loc{0, 0}
	b.selection.End = Loc{len(b.lines) - 1, len(b.lines[len(b.lines)-1])}
	b.cursor = b.selection.End
}
func (b *Buffer) getSelectedText() string {
	if !b.HasSelection() { return "" }
	s, e := b.getSelectionRange()
	if s.L == e.L { return string(b.lines[s.L][s.C:e.C]) }
	var sb strings.Builder
	sb.WriteString(string(b.lines[s.L][s.C:])); sb.WriteByte('\n')
	for i := s.L + 1; i < e.L; i++ { sb.WriteString(string(b.lines[i])); sb.WriteByte('\n') }
	sb.WriteString(string(b.lines[e.L][:e.C]))
	return sb.String()
}
func isWordChar(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
func (b *Buffer) selectWordAtCursor() {
	line := b.lines[b.cursor.L]
	if len(line) == 0 { return }
	c := b.cursor.C
	if c >= len(line) { c = len(line) - 1 }
	isSp := unicode.IsSpace(line[c]); isAlpha := isWordChar(line[c])
	left := c
	for left > 0 {
		pr := line[left-1]
		if isSp { if !unicode.IsSpace(pr) { break } } else { if unicode.IsSpace(pr) || isWordChar(pr) != isAlpha { break } }
		left--
	}
	right := c
	for right < len(line) {
		cr := line[right]
		if isSp { if !unicode.IsSpace(cr) { break } } else { if unicode.IsSpace(cr) || isWordChar(cr) != isAlpha { break } }
		right++
	}
	b.isSelecting = true; b.selection.Start = Loc{b.cursor.L, left}; b.selection.End = Loc{b.cursor.L, right}; b.cursor = b.selection.End
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
		if b.cursor.L > 0 { b.cursor.L--; b.cursor.C = len(b.lines[b.cursor.L]) }
		return
	}
	line := b.lines[b.cursor.L]
	for b.cursor.C > 0 && unicode.IsSpace(line[b.cursor.C-1]) { b.cursor.C-- }
	if b.cursor.C == 0 { return }
	isAlpha := isWordChar(line[b.cursor.C-1])
	for b.cursor.C > 0 && !unicode.IsSpace(line[b.cursor.C-1]) && isWordChar(line[b.cursor.C-1]) == isAlpha { b.cursor.C-- }
}
func (b *Buffer) moveWordRight() {
	line := b.lines[b.cursor.L]
	if b.cursor.C == len(line) {
		if b.cursor.L < len(b.lines)-1 { b.cursor.L++; b.cursor.C = 0 }
		return
	}
	isAlpha := isWordChar(line[b.cursor.C])
	for b.cursor.C < len(line) && !unicode.IsSpace(line[b.cursor.C]) && isWordChar(line[b.cursor.C]) == isAlpha { b.cursor.C++ }
	for b.cursor.C < len(line) && unicode.IsSpace(line[b.cursor.C]) { b.cursor.C++ }
}
func (b *Buffer) moveParagraphUp() {
	curr := b.cursor.L - 1
	for curr >= 0 && len(b.lines[curr]) == 0 { curr-- }
	for curr >= 0 && len(b.lines[curr]) > 0 { curr-- }
	if curr < 0 { b.cursor.L = 0 } else { b.cursor.L = curr }
	b.cursor.C = 0
}
func (b *Buffer) moveParagraphDown() {
	curr := b.cursor.L + 1
	for curr < len(b.lines) && len(b.lines[curr]) == 0 { curr++ }
	for curr < len(b.lines) && len(b.lines[curr]) > 0 { curr++ }
	if curr >= len(b.lines) { b.cursor.L = len(b.lines) - 1 } else { b.cursor.L = curr }
	b.cursor.C = 0
}
func (b *Buffer) isLineUnwrapped(lineIdx int, cfg Config) bool {
	if lineIdx < 0 || lineIdx >= len(b.lines) {
		return false
	}
	if !cfg.LineWrapping {
		return true
	}
	// 💡 Cap이 0보다 클 때만 제한을 걸고, 0이면 무조건 false(랩핑 유지)를 반환!
	if cfg.LineWrappingCap > 0 && len(b.lines[lineIdx]) > cfg.LineWrappingCap {
		return true
	}
	return false
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
func (b *Buffer) generateVisualLines(maxWidth int, cfg Config) []VisualLine {
	lineNumWidth := b.getLineNumWidth(cfg)
	textMaxWidth := maxWidth - lineNumWidth
	if textMaxWidth <= 0 { textMaxWidth = 1 }

	// 설정이나 너비가 바뀌면 전체 캐시 리셋
	if b.cachedMaxWidth != maxWidth || b.cachedConfig.LineWrapping != cfg.LineWrapping || b.cachedConfig.TabSize != cfg.TabSize || b.cachedConfig.LineWrappingCap != cfg.LineWrappingCap {
		for i := range b.vCache { b.vCache[i] = nil }
		b.cachedMaxWidth = maxWidth
		b.cachedConfig = cfg
	}

	total := 0
	for i, line := range b.lines {
	if b.vCache[i] == nil {
			var temp []VisualLine
			// 💡 Cap이 0보다 클 때만 초과 검사를 실행! (0이면 제한 없이 랩핑)
			if !cfg.LineWrapping || len(line) == 0 || (cfg.LineWrappingCap > 0 && len(line) > cfg.LineWrappingCap) {
				temp = []VisualLine{{lineIdx: i, isWrapped: false, startCX: 0, endCX: len(line)}}
			} else {
				start, currentX := 0, 0
				for cx, r := range line {
					rw := runewidth.RuneWidth(r)
					if r == '\t' { rw = cfg.TabSize }
					if currentX+rw > textMaxWidth {
						temp = append(temp, VisualLine{lineIdx: i, isWrapped: start > 0, startCX: start, endCX: cx})
						start = cx; currentX = 0
					}
					currentX += rw
				}
				temp = append(temp, VisualLine{lineIdx: i, isWrapped: start > 0, startCX: start, endCX: len(line)})
			}
			b.vCache[i] = temp
		}
		total += len(b.vCache[i])
	}

	// 💡 미리 크기를 할당하여 메모리 복사 속도 극대화
	vLines := make([]VisualLine, 0, total)
	for i, c := range b.vCache {
		for j := range c {
			c[j].lineIdx = i // 인덱스 밀림 완벽 보정
			vLines = append(vLines, c[j])
		}
	}

	b.cachedVLines = vLines
	b.vLinesValid = true
	b.totalVLines = total
	return vLines
}

func (b *Buffer) getVisualLineCount(cfg Config) int {
	if !b.vLinesValid { return len(b.lines) }
	return b.totalVLines
}

func (b *Buffer) getVisualLine(vIdx int, cfg Config) VisualLine {
	if vIdx < 0 || vIdx >= len(b.cachedVLines) { return VisualLine{} }
	return b.cachedVLines[vIdx]
}

func (b *Buffer) getVCursorIdx(cfg Config) int {
	b.cursor = b.clampLoc(b.cursor) // 💡 방어 코드: 커서가 범위를 넘으면 자동 보정
	for i, vl := range b.cachedVLines {
		if vl.lineIdx == b.cursor.L {
			if b.cursor.C >= vl.startCX && b.cursor.C <= vl.endCX {
				if b.cursor.C == vl.endCX && i+1 < len(b.cachedVLines) && b.cachedVLines[i+1].lineIdx == b.cursor.L { continue }
				return i
			}
		}
	}
	return 0
}

func (b *Buffer) scrollToCursorV(cfg Config, screenHeight int) {
	if screenHeight <= 0 || len(b.cachedVLines) == 0 { return }
	b.cursor = b.clampLoc(b.cursor) // 💡 방어 코드
	vCursorIdx := b.getVCursorIdx(cfg)
	if vCursorIdx < b.vOffsetIdx { b.vOffsetIdx = vCursorIdx }
	if vCursorIdx >= b.vOffsetIdx+screenHeight { b.vOffsetIdx = vCursorIdx - screenHeight + 1 }
	if b.vOffsetIdx >= b.totalVLines { b.vOffsetIdx = b.totalVLines - 1 }
	if b.vOffsetIdx < 0 { b.vOffsetIdx = 0 }
}

func (b *Buffer) getCursorVisualRange(cfg Config) (int, int) {
	b.cursor = b.clampLoc(b.cursor)
	line := b.lines[b.cursor.L]

	// 1. Calculate cursor visual X
	cursorVisX := 0
	for i := 0; i < b.cursor.C && i < len(line); i++ {
		r := line[i]
		rw := runewidth.RuneWidth(r)
		if r == '\t' { rw = cfg.TabSize }
		cursorVisX += rw
	}

	startVisX := cursorVisX
	endVisX := cursorVisX

	// 2. If there's an active selection/match on the current line, expand visual boundaries
	if b.isSelecting {
		selStart, selEnd := b.getSelectionRange()
		if selStart.L == b.cursor.L || selEnd.L == b.cursor.L {
			startCX := b.cursor.C
			endCX := b.cursor.C

			if selStart.L == b.cursor.L {
				startCX = selStart.C
			} else {
				startCX = 0
			}

			if selEnd.L == b.cursor.L {
				endCX = selEnd.C
			} else {
				endCX = len(line)
			}

			vStart := 0
			for i := 0; i < startCX && i < len(line); i++ {
				r := line[i]
				rw := runewidth.RuneWidth(r)
				if r == '\t' { rw = cfg.TabSize }
				vStart += rw
			}

			vEnd := 0
			for i := 0; i < endCX && i < len(line); i++ {
				r := line[i]
				rw := runewidth.RuneWidth(r)
				if r == '\t' { rw = cfg.TabSize }
				vEnd += rw
			}

			startVisX = vStart
			endVisX = vEnd
		}
	}

	return startVisX, endVisX
}

func (b *Buffer) scrollToCursorH(textMaxWidth int, cfg Config) {
	if textMaxWidth <= 0 { return }
	startVisX, endVisX := b.getCursorVisualRange(cfg)

	if endVisX >= b.hOffset+textMaxWidth {
		b.hOffset = endVisX - textMaxWidth + 1
	}
	if startVisX < b.hOffset {
		b.hOffset = startVisX
	}
}

// 마우스 클릭 시 화면 좌표를 실제 데이터 좌표(Loc)로 변환
func (b *Buffer) screenToMemoryPosV(vLines []VisualLine, mx, my, tabHeight int, cfg Config) Loc {
	lineNumWidth := b.getLineNumWidth(cfg)
	
	if len(vLines) == 0 || len(b.lines) == 0 { return Loc{0, 0} } // 💡 방어 코드: 버퍼가 비어있을 때
	
	targetVIdx := b.vOffsetIdx + (my - tabHeight)
	if targetVIdx >= len(vLines) { targetVIdx = len(vLines) - 1 }
	if targetVIdx < 0 { return Loc{0, 0} }

	vl := vLines[targetVIdx]
	
	// 💡 핵심 방어: 삭제 키 연타 등으로 인해 화면 캐시가 예전 줄을 가리키고 있을 경우 (Stale Cache 에러 방지)
	if vl.lineIdx >= len(b.lines) {
		lastL := len(b.lines) - 1
		return Loc{lastL, len(b.lines[lastL])}
	}
	
	relativeX := mx - lineNumWidth + b.hOffset
	if relativeX <= 0 { return Loc{vl.lineIdx, vl.startCX} }

	currentX := 0
	line := b.lines[vl.lineIdx]
	
	// 💡 i < len(line) 검사 추가: 줄 길이가 짧아졌을 때 접근 차단
	for i := vl.startCX; i < vl.endCX && i < len(line); i++ {
		r := line[i]
		rw := runewidth.RuneWidth(r)
		if r == '\t' { rw = cfg.TabSize }
		if currentX+rw/2 >= relativeX { return Loc{vl.lineIdx, i} }
		currentX += rw
	}
	
	endC := vl.endCX
	if endC > len(line) { endC = len(line) } // 💡 끝 좌표 보정
	return Loc{vl.lineIdx, endC}
}


func sprintfRight(num, width int) string {
	res := ""
	n := num
	for n > 0 { res = string(rune('0'+(n%10))) + res; n /= 10 }
	if num == 0 { res = "0" }
	for len(res) < width { res = " " + res }
	return res
}

// 💡 추가됨: 파일명이 겹치면 부모 폴더까지 보여주는 스마트 타이틀 함수
func (e *Editor) getTabTitle(bufIdx int) string {
	b := e.buffers[bufIdx]
	if b.filePath == "" {
		if b.isConfig { return "config.json" }
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

func (e *Editor) draw(s tcell.Screen, vLines []VisualLine) {
	w, h := s.Size()
	if h <= 0 || w <= 0 { return }

	b := e.getActive()
	if e.activeBuffer != e.prevActiveBuf || b.vOffsetIdx != e.prevVOffset || b.hOffset != e.prevHOffset ||
		e.paletteActive != e.prevPalette || e.ctxMenuActive != e.prevCtxMenu || e.encodeMenuActive != e.prevEncode ||
		e.promptMode != e.prevPrompt || b.searchMode != e.prevSearch || b.gotoMode != e.prevGoto || b.isReplace != e.prevReplace || len(b.lines) != e.prevLinesLen {
			e.needsFullRefresh = true
		}

if e.needsFullRefresh || w != e.prevW || h != e.prevH {
			s.Clear()
			e.screenCache = make([][]cellState, h)
			e.nextCache = make([][]cellState, h) // 💡 렌더링 버퍼 영구 할당
			for y := 0; y < h; y++ {
				e.screenCache[y] = make([]cellState, w)
				e.nextCache[y] = make([]cellState, w) // 💡 타입 수정됨
				for x := 0; x < w; x++ { e.screenCache[y][x] = cellState{mainc: ' ', style: tcell.StyleDefault} }
			}
			e.prevW = w; e.prevH = h; e.needsFullRefresh = false
		}

		// 💡 매 프레임마다 배열을 버리지 않고 기존 배열 내용을 초기화하여 재사용 (Zero Allocation)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ { e.nextCache[y][x] = cellState{mainc: ' ', style: tcell.StyleDefault} }
		}

		setCell := func(x, y int, r rune, comb []rune, style tcell.Style) {
			if x >= 0 && x < w && y >= 0 && y < h { e.nextCache[y][x] = cellState{mainc: r, comb: comb, style: style} }
		}

		if e.cfg.LineWrapping && !b.isLineUnwrapped(b.cursor.L, e.cfg) { b.hOffset = 0 }
		lineNumWidth := b.getLineNumWidth(e.cfg)
		textMaxWidth := w - lineNumWidth
		if textMaxWidth <= 0 { textMaxWidth = 1 }

		tabStyle := tcell.StyleDefault.Background(tcell.ColorDarkGray).Foreground(tcell.ColorWhite)
		activeTabStyle := tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorWhite).Bold(true)

		e.tabBounds = []TabBound{}
		tabX, tabY := 0, 0

		for i, buf := range e.buffers {
			name := e.getTabTitle(i) // 💡 스마트 타이틀 함수 호출
			
			if buf.isModified { name += " *" }
			if buf.isReadOnly { name += " 🔒" } // 💡 탭에 읽기 전용 표시
			tabStr := " [" + name + "] "
			tabLen := runewidth.StringWidth(tabStr)

			if tabX+tabLen > w {
				for x := tabX; x < w; x++ { setCell(x, tabY, ' ', nil, tabStyle) }
				tabX = 0; tabY++
			}
			currentStyle := tabStyle; if i == e.activeBuffer { currentStyle = activeTabStyle }
			startX := tabX
			for _, r := range tabStr { setCell(tabX, tabY, r, nil, currentStyle); tabX += runewidth.RuneWidth(r) }
			e.tabBounds = append(e.tabBounds, TabBound{Idx: i, StartX: startX, EndX: tabX, Y: tabY})
		}
		for x := tabX; x < w; x++ { setCell(x, tabY, ' ', nil, tabStyle) }
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
			hasSel :=  b.HasSelection()
			totalVLines := b.getVisualLineCount(e.cfg)
			for vIdx := b.vOffsetIdx; vIdx < totalVLines; vIdx++ {
				if currentRenderY >= h-1 { break }
				vl := b.getVisualLine(vIdx, e.cfg)
				lineIdx := vl.lineIdx
				lineData := b.lines[lineIdx]

				// 💡 최적화: 이 라인에 해당하는 검색 매치들을 미리 이진 탐색으로 추출
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
				if e.cfg.HighlightLine && lineIdx == b.cursor.L && !b.isSelecting { lineStyle = highlightStyle }
				for x := lineNumWidth; x < w; x++ { setCell(x, currentRenderY, ' ', nil, lineStyle) }

				if e.cfg.ShowLineNumbers {
					if !vl.isWrapped {
						lineNumStr := sprintfRight(lineIdx+1, lineNumWidth-1) + " "
						for x, r := range lineNumStr { setCell(x, currentRenderY, r, nil, lineNumStyle) }
					} else { for x := 0; x < lineNumWidth; x++ { setCell(x, currentRenderY, ' ', nil, lineNumStyle) } }
				}

				currentX := 0
				if len(lineData) == 0 && lineIdx == b.cursor.L {
					cursorVX = lineNumWidth - b.hOffset
					cursorVY = currentRenderY
				}

				for i := vl.startCX; i < vl.endCX; i++ {
					r := lineData[i]
					rw := runewidth.RuneWidth(r)
					if r == '\t' { rw = e.cfg.TabSize }

					if lineIdx == b.cursor.L && i == b.cursor.C {
						cursorVX = lineNumWidth + currentX - b.hOffset; cursorVY = currentRenderY
					}

					charStyle := lineStyle
					if len(lineMatches) > 0 {
						for _, m := range lineMatches {
							if i >= m.loc.C && i < m.loc.C+m.matchLen {
								charStyle = matchHighlightStyle; break
							}
						}
					}

					if hasSel {
						selected := false
						if lineIdx > selStart.L && lineIdx < selEnd.L { selected = true }
						if lineIdx == selStart.L && lineIdx == selEnd.L { selected = i >= selStart.C && i < selEnd.C }
						if lineIdx == selStart.L && lineIdx < selEnd.L { selected = i >= selStart.C }
						if lineIdx == selEnd.L && lineIdx > selStart.L { selected = i < selEnd.C }
						if selected { charStyle = selectedStyle }
					}

					if currentX >= b.hOffset && currentX < b.hOffset+textMaxWidth {
						if r == '\t' {
							for tx := 0; tx < rw; tx++ {
								if currentX+tx >= b.hOffset && currentX+tx < b.hOffset+textMaxWidth { setCell(lineNumWidth+currentX+tx-b.hOffset, currentRenderY, ' ', nil, charStyle) }
							}
						} else { setCell(lineNumWidth+currentX-b.hOffset, currentRenderY, r, nil, charStyle) }
					}
					currentX += rw
				}

				if !vl.isWrapped || vl.endCX == len(lineData) {
					if hasSel {
						selected := false
						i := len(lineData)
						if lineIdx > selStart.L && lineIdx < selEnd.L { selected = true }
						if lineIdx == selStart.L && lineIdx == selEnd.L { selected = i >= selStart.C && i < selEnd.C }
						if lineIdx == selStart.L && lineIdx < selEnd.L { selected = i >= selStart.C }
						if lineIdx == selEnd.L && lineIdx > selStart.L { selected = i < selEnd.C }
						if selected {
							if currentX >= b.hOffset && currentX < b.hOffset+textMaxWidth { setCell(lineNumWidth+currentX-b.hOffset, currentRenderY, ' ', nil, selectedStyle) }
						}
					}
				}

				if lineIdx == b.cursor.L && b.cursor.C == vl.endCX {
					if b.cursor.C == len(lineData) || vIdx+1 >= len(vLines) || vLines[vIdx+1].lineIdx != b.cursor.L {
						cursorVX = lineNumWidth + currentX - b.hOffset; cursorVY = currentRenderY
					}
				}

				// 💡 Left/Right scroll indicators for unwrapped lines
				if b.isLineUnwrapped(lineIdx, e.cfg) {
					indicatorStyle := lineStyle.Foreground(tcell.ColorDarkCyan).Bold(true)
					if b.hOffset > 0 {
						if lineNumWidth > 0 {
							setCell(lineNumWidth-1, currentRenderY, '<', nil, indicatorStyle)
						} else {
							setCell(0, currentRenderY, '<', nil, indicatorStyle)
						}
					}
					// Calculate total visual width of this line
					totalWidth := 0
					for _, r := range lineData {
						rw := runewidth.RuneWidth(r)
						if r == '\t' { rw = e.cfg.TabSize }
						totalWidth += rw
					}
					if totalWidth > b.hOffset+textMaxWidth {
						setCell(w-1, currentRenderY, '>', nil, indicatorStyle)
					}
				}

				currentRenderY++
			}

			if e.promptMode {
				var promptMsg string
				if e.promptType == "quit" { promptMsg = " [Warning] 저장되지 않은 탭이 있습니다. 무시하고 종료할까요? (y/n)"
				} else if e.promptType == "close" { promptMsg = " [Warning] 변경된 내용이 있습니다. 탭을 닫을까요? (y/n)"
				} else if e.promptType == "reset_config" { promptMsg = " [Warning] 설정을 기본값으로 초기화하시겠습니까? (y/n)"
				} else if e.promptType == "reopen" { promptMsg = " [Warning] 변경된 내용이 있습니다. 무시하고 다시 열까요? (y/n)"
				} else if e.promptType == "close_config" { promptMsg = " [Warning] 설정이 저장되지 않았습니다. 무시하고 닫을까요? (y/n)"
				} else if e.promptType == "external_change" { promptMsg = " [Warning] 파일이 외부에서 변경되었습니다. 디스크 내용으로 덮어쓸까요? (y/n)" 
				} else if e.promptType == "alert" { promptMsg = " [Alert] " + e.alertMessage + " (Enter/Esc)" }
				
				promptStyle := tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true)

				currentX := 0
				for _, r := range promptMsg {
					if currentX < w { setCell(currentX, h-1, r, nil, promptStyle); currentX += runewidth.RuneWidth(r) }
				}
				for currentX < w { setCell(currentX, h-1, ' ', nil, promptStyle); currentX++ }
				cursorVX = runewidth.StringWidth(promptMsg); cursorVY = h - 1

			} else if b.searchMode || b.gotoMode {
				closeBtn := " [X] "
				b.closeBtnStartX = w - len(closeBtn)
				b.closeBtnEndX = w - 1

				checkboxArea := ""
				if !b.gotoMode {
					cbRegex := "[ ] .*"; if b.searchRegex { cbRegex = "[x] .*" }
					cbCase := "[ ] Aa"; if b.searchCase { cbCase = "[x] Aa" }
					cbWord := "[ ] \\b"; if b.searchWord { cbWord = "[x] \\b" }
					checkboxArea = " " + cbRegex + "  " + cbCase + "  " + cbWord + " "
				}
				cbLen := runewidth.StringWidth(checkboxArea)
				cbStartX := b.closeBtnStartX - cbLen

				if !b.gotoMode {
					b.chkRegexX1 = cbStartX + 1; b.chkRegexX2 = b.chkRegexX1 + 6
					b.chkCaseX1 = b.chkRegexX2 + 2; b.chkCaseX2 = b.chkCaseX1 + 6
					b.chkWordX1 = b.chkCaseX2 + 2; b.chkWordX2 = b.chkWordX1 + 6
				}

				var prefix, suffix string
				var targetStr *[]rune

				if b.gotoMode {
					prefix = " [Go To] Line,Col: "; targetStr = &b.gotoInput; suffix = "  (Enter: 이동, Esc: 취소)"
				} else if b.isReplace {
					if b.replaceStep == 1 { prefix = " [Replace] Find: "; targetStr = &b.searchQuery
					} else if b.replaceStep == 2 { prefix = " [Replace] Find: " + string(b.searchQuery) + "  ➔ Replace: "; targetStr = &b.replaceQuery
					} else {
						matchCountStr := "0/0"
						if len(b.matches) > 0 {
							totStr := sprintfRight(len(b.matches), 0)
							if b.searchCapped { totStr += "+" }
							matchCountStr = sprintfRight(b.matchIdx+1, 0) + "/" + totStr
						}
						prefix = " [Replace] Find: " + string(b.searchQuery) + "  ➔ Replace: " + string(b.replaceQuery) + "  [" + matchCountStr + "] (Enter:바꾸기, Up:이전, Down:건너뛰기, Ctrl+A:모두)"
						targetStr = nil
					}
				} else {
					matchCountStr := "0/0"
					if len(b.matches) > 0 {
						totStr := sprintfRight(len(b.matches), 0)
						if b.searchCapped { totStr += "+" }
						matchCountStr = sprintfRight(b.matchIdx+1, 0) + "/" + totStr
					}
					prefix = " [Find] Search: "; targetStr = &b.searchQuery; suffix = "  [" + matchCountStr + "] (Enter/Down:다음, Up:이전)"
				}

				currentX := 0
				for _, r := range prefix {
					if currentX < cbStartX { setCell(currentX, h-1, r, nil, statusStyle); currentX += runewidth.RuneWidth(r) }
				}

				inputStartX := currentX
				if targetStr != nil {
					selStart, selEnd := b.inputSelStart, b.inputSelEnd
					if selStart > selEnd { selStart, selEnd = selEnd, selStart }

					for i, r := range *targetStr {
						style := statusStyle
						if b.isInputSelect && i >= selStart && i < selEnd { style = selectedStyle }
						if currentX < cbStartX { setCell(currentX, h-1, r, nil, style); currentX += runewidth.RuneWidth(r) }
					}
					cursorVX = inputStartX + runewidth.StringWidth(string((*targetStr)[:b.inputCX]))
				} else { cursorVX = -1 }

				for _, r := range suffix {
					if currentX < cbStartX { setCell(currentX, h-1, r, nil, statusStyle); currentX += runewidth.RuneWidth(r) }
				}
				for currentX < cbStartX { setCell(currentX, h-1, ' ', nil, statusStyle); currentX++ }
				cx := cbStartX
				for _, r := range checkboxArea { setCell(cx, h-1, r, nil, statusStyle); cx += runewidth.RuneWidth(r) }
				cursorVY = h - 1

				closeStyle := tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true)
				for i, r := range closeBtn {
					if b.closeBtnStartX+i < w { setCell(b.closeBtnStartX+i, h-1, r, nil, closeStyle) }
				}

			} else {
				modeName := b.encoding
				if b.isConfig { modeName = "CONFIG.JSON" }
				charCountStr := ""
				if hasSel {
					selChars := 0
					if selStart.L == selEnd.L {
						selChars = selEnd.C - selStart.C
					} else {
						// 첫 번째 줄 선택된 글자 수 + 줄바꿈(\n)
						selChars += len(b.lines[selStart.L]) - selStart.C + 1
						// 중간에 낀 줄들의 전체 글자 수 + 줄바꿈(\n)
						for idx := selStart.L + 1; idx < selEnd.L; idx++ {
							selChars += len(b.lines[idx]) + 1
						}
						// 마지막 줄 선택된 글자 수
						selChars += selEnd.C
					}
					charCountStr = fmt.Sprintf("%d Sel", selChars)
				} else {
					charCountStr = fmt.Sprintf("%d Chars", b.totalChars)
				}

			prefix := " [Ctrl+P] Command Palette | "
				encodeStr := "Encode:" + modeName
				
				roStr := ""
				if b.isReadOnly { roStr = " | 🔒 READONLY" }

				// 💡 전체 경로 표시 (너무 길면 앞부분 생략)
				displayPath := b.filePath
				if displayPath == "" { 
					displayPath = "New Buffer" 
				} else {
					// 💡 바이트(Byte)가 아닌 글자(Rune) 단위로 변환 후 잘라야 한글이 깨지지 않습니다!
					pathRunes := []rune(displayPath)
					if len(pathRunes) > 50 {
						displayPath = "..." + string(pathRunes[len(pathRunes)-47:])
					}
				}

				// 💡 요청하신 순서대로 나열: 글자수 | Ln, Col | 전체 경로 | 읽기 전용 여부
				suffix := fmt.Sprintf(" | %s | Ln %d, Col %d | %s%s ", charCountStr, b.cursor.L+1, b.cursor.C+1, displayPath, roStr)

				// 그리기 시작
				currentX := 0
				for _, r := range prefix {
					if currentX < w { setCell(currentX, h-1, r, nil, statusStyle); currentX += runewidth.RuneWidth(r) }
				}
				b.encodeBtnX1 = currentX
				encodeStyle := tcell.StyleDefault.Background(tcell.ColorDarkCyan).Foreground(tcell.ColorWhite).Bold(true)
				for _, r := range encodeStr {
					if currentX < w { setCell(currentX, h-1, r, nil, encodeStyle); currentX += runewidth.RuneWidth(r) }
				}
				b.encodeBtnX2 = currentX - 1
				for _, r := range suffix {
					if currentX < w { setCell(currentX, h-1, r, nil, statusStyle); currentX += runewidth.RuneWidth(r) }
				}
				
				// 남은 빈칸 지우기
				for currentX < w { setCell(currentX, h-1, ' ', nil, statusStyle); currentX++ }
			} // 💡 에러의 원인이었던 닫는 중괄호가 여기에 안전하게 들어있습니다!
			// 💡 이전 에러의 원인이었던 닫는 중괄호가 여기에 안전하게 들어있습니다!
		
			drawMenu := func(isActive bool, title string, items []PaletteItem, cursor, mx, my int, outX, outY, outW, outH *int) {
				if !isActive { return }
				pWidth := 40
				if title == " Command Palette " { pWidth = 60 }
				pHeight := len(items) + 2
				if pHeight > h-4 { pHeight = h - 4 }

				pX, pY := mx, my
				if title == " Command Palette " { pX = (w - pWidth) / 2; pY = (h - pHeight) / 2 }
				if pX < 0 { pX = 0 }; if pY < 0 { pY = 0 }
				if pX+pWidth > w { pX = w - pWidth }
				if pY+pHeight > h { pY = h - pHeight }
				*outX, *outY, *outW, *outH = pX, pY, pWidth, pHeight

				marginStyle := tcell.StyleDefault.Background(tcell.ColorDefault).Foreground(tcell.ColorDefault)
				for y := -1; y <= pHeight; y++ {
					for x := -1; x <= pWidth; x++ {
						if pX+x >= 0 && pX+x < w && pY+y >= 0 && pY+y < h { setCell(pX+x, pY+y, ' ', nil, marginStyle) }
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
							if y == 0 && x > 0 && x < pWidth-1 { r = '─' }
							if y == pHeight-1 && x > 0 && x < pWidth-1 { r = '─' }
							if x == 0 && y > 0 && y < pHeight-1 { r = '│' }
							if x == pWidth-1 && y > 0 && y < pHeight-1 { r = '│' }
							if x == 0 && y == 0 { r = '┌' }
							if x == pWidth-1 && y == 0 { r = '┐' }
							if x == 0 && y == pHeight-1 { r = '└' }
							if x == pWidth-1 && y == pHeight-1 { r = '┘' }
						}
						setCell(pX+x, pY+y, r, nil, style)
					}
				}

				if title != "" {
					tx := pX + (pWidth-runewidth.StringWidth(title))/2
					for i, r := range title { setCell(tx+i, pY, r, nil, borderStyle) }
				}

				visibleItems := pHeight - 2
				startIdx := cursor - visibleItems/2
				if startIdx < 0 { startIdx = 0 }
				if startIdx+visibleItems > len(items) { startIdx = len(items) - visibleItems; if startIdx < 0 { startIdx = 0 } }

				for i := 0; i < visibleItems && startIdx+i < len(items); i++ {
					idx := startIdx + i
					item := items[idx]
					style := itemStyle
					if idx == cursor { style = selectedStyle }

					for x := 1; x < pWidth-1; x++ { setCell(pX+x, pY+1+i, ' ', nil, style) }

					strName := " " + item.Name
					strShortcut := item.Shortcut + " "
					cx := pX + 1
					for _, r := range strName { setCell(cx, pY+1+i, r, nil, style); cx += runewidth.RuneWidth(r) }
					scLen := runewidth.StringWidth(strShortcut)
					cx = pX + pWidth - 1 - scLen
					for _, r := range strShortcut { setCell(cx, pY+1+i, r, nil, style); cx += runewidth.RuneWidth(r) }
				}
				cursorVX = -1
			}

			drawMenu(e.paletteActive, " Command Palette ", e.paletteItems, e.paletteCursor, 0, 0, &e.paletteX, &e.paletteY, &e.paletteW, &e.paletteH)

			drawMenu(e.ctxMenuActive, "", e.ctxMenuItems, e.ctxMenuCursor, e.ctxMenuX, e.ctxMenuY, &e.ctxMenuX, &e.ctxMenuY, &e.ctxMenuW, &e.ctxMenuH)
			drawMenu(e.encodeMenuActive, " Select Encoding ", e.encodeMenuItems, e.encodeMenuCursor, e.encodeMenuX, e.encodeMenuY, &e.encodeMenuX, &e.encodeMenuY, &e.encodeMenuW, &e.encodeMenuH)

			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					c1 := e.screenCache[y][x]
					c2 := e.nextCache[y][x] // 💡 구조체의 멤버로 참조하도록 e. 추가
					if c1.mainc != c2.mainc || c1.style != c2.style || !runeSliceEqual(c1.comb, c2.comb) {
						s.SetContent(x, y, c2.mainc, c2.comb, c2.style)
						e.screenCache[y][x] = c2
					}
				}
			}

			e.prevActiveBuf = e.activeBuffer; e.prevVOffset = b.vOffsetIdx; e.prevHOffset = b.hOffset
			e.prevPalette = e.paletteActive; e.prevCtxMenu = e.ctxMenuActive; e.prevEncode = e.encodeMenuActive
			e.prevPrompt = e.promptMode; e.prevSearch = b.searchMode; e.prevGoto = b.gotoMode
			e.prevReplace = b.isReplace; e.prevLinesLen = len(b.lines)

			if cursorVX >= lineNumWidth && cursorVX < w && cursorVY >= 0 && cursorVY < h {
				s.ShowCursor(cursorVX, cursorVY)
			} else { s.HideCursor() }
			s.Show()
}

func runeSliceEqual(a, b []rune) bool {
	if len(a) != len(b) { return false }
	for i := range a { if a[i] != b[i] { return false } }
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

	if err != nil || filePath == "" { return }

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
	if err != nil { return }

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
		if err := json.Unmarshal([]byte(content), &newCfg); err == nil { e.cfg = newCfg }
	}
}

func (e *Editor) saveAsFile(s tcell.Screen) {
	b := e.getActive()
	s.Suspend()
	filePath, err := zenity.SelectFileSave(zenity.Title("다른 이름으로 저장"), zenity.ConfirmOverwrite())
	s.Resume()

	// 💡 FIX: GUI 창이 닫힌 직후 터미널 화면 강제 전체 리프레시!
	s.Sync()
	e.needsFullRefresh = true

	if err != nil || filePath == "" { return }
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
	// 💡 새 경로를 파일 감지기에 등록
	e.fileWatcher.Add(filePath)

	if b.isConfig {
		var newCfg Config
		if err := json.Unmarshal([]byte(content), &newCfg); err == nil { e.cfg = newCfg }
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
			if buf.filePath != "" { e.fileWatcher.Remove(buf.filePath) }
			e.buffers = append(e.buffers[:i], e.buffers[i+1:]...)
			e.activeBuffer = 0
			return
		}
	}
	path := getConfigPath()
	if path == "" { return }
	strData, _, err := readFileDetectEncoding(path)
	if err != nil { return }

	configBuf := NewBuffer()
	configBuf.filePath = path
	configBuf.isConfig = true

	// 💡 config.json 파일도 감지기에 등록
	e.fileWatcher.Add(path)

	configBuf.setLinesFromText(strData)
	configBuf.setLinesFromText(strData)
	configBuf.savedContent = configBuf.getContent()
	configBuf.savedTotalChars = configBuf.totalChars // 💡 추가됨
	e.buffers = append(e.buffers, configBuf)
	e.activeBuffer = len(e.buffers) - 1
}



		// --- [ 5. 검색 및 치환 로직 ] ---

// 💡 거리 계산용 헬퍼 함수
		func absInt(n int) int {
			if n < 0 { return -n }
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
				if idx == 0 { return 0 }
				if idx == len(b.matches) { return len(b.matches) - 1 }

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
				if idx-1 >= 0 { return idx - 1 }
				return len(b.matches) - 1
			}
			if idx < len(b.matches) { return idx }
			return 0
		}

		// 💡 O(N) 전체 탐색을 버리고 커서 기준 위/아래 양방향(O(K))으로 뻗어나가는 극한 최적화 검색 엔진
		func (b *Buffer) findAllMatches(overlap bool) {
			b.matches = []MatchInfo{}
			b.matchIdx = -1
			b.searchCapped = false
			query := string(b.searchQuery)
			if query == "" { b.clearSelection(); return }

			var re *regexp.Regexp
			if b.searchRegex || b.searchWord {
				pattern := query
				if !b.searchRegex { pattern = regexp.QuoteMeta(query) }
				if b.searchWord { pattern = `\b` + pattern + `\b` }
				if !b.searchCase { pattern = "(?i)" + pattern }
				compiled, err := regexp.Compile(pattern)
				if err == nil { re = compiled }
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
				line := b.lines[lineIdx]
				if re != nil {
					lineStr := string(line)
					locs := re.FindAllStringIndex(lineStr, -1)
					for _, loc := range locs {
						runeStart := utf8.RuneCountInString(lineStr[:loc[0]])
						runeLen := utf8.RuneCountInString(lineStr[loc[0]:loc[1]])
						lineMatches = append(lineMatches, MatchInfo{loc: Loc{lineIdx, runeStart}, matchLen: runeLen})
					}
				} else {
					targetRunes := line
					for c := 0; c <= len(targetRunes)-len(searchRunes); {
						match := true
						for j := 0; j < len(searchRunes); j++ {
							tr := targetRunes[c+j]
							sr := searchRunes[j]
							if !b.searchCase { tr = unicode.ToLower(tr) }
							if tr != sr { match = false; break }
						}
						if match {
							lineMatches = append(lineMatches, MatchInfo{loc: Loc{lineIdx, c}, matchLen: len(searchRunes)})
							if !overlap { c += len(searchRunes) } else { c++ }
						} else { c++ }
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
				if countBelow >= 5000 { b.searchCapped = true; break }
				lm := findInLine(i)
				if len(lm) > 0 {
					matchesBelow = append(matchesBelow, lm...)
					countBelow += len(lm)
				}
			}

			// 3. 위 방향 탐색 (O(K))
			for i := b.cursor.L - 1; i >= 0; i-- {
				if countAbove >= 5000 { b.searchCapped = true; break }
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
			if b.matchIdx < 0 || b.matchIdx >= len(b.matches) { return }
			m := b.matches[b.matchIdx]
			b.cursor = m.loc
			b.isSelecting = true
			b.selection.Start = m.loc
			b.selection.End = Loc{m.loc.L, m.loc.C + m.matchLen}
		}

		func (b *Buffer) replaceCurrent(overlap bool) {
			if b.matchIdx < 0 || b.matchIdx >= len(b.matches) { return }
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
			} else { b.matchIdx = -1 }
		}

		type EditorAction func(e *Editor, s tcell.Screen)

		var ActionMap = map[tcell.Key]EditorAction{
			tcell.KeyCtrlG: func(e *Editor, s tcell.Screen) {
				b := e.getActive(); b.clearSelection(); b.searchMode = false; b.gotoMode = true
				b.gotoInput = []rune{}; b.inputCX = 0; b.isInputSelect = false
			},
		tcell.KeyF5: func(e *Editor, s tcell.Screen) {
				b := e.getActive(); if b.isReadOnly { return }
				b.BeginTransaction(); b.DeleteSelection()
				goLayout := convertLinuxDateToGoLayout(e.cfg.DateFormat)
				b.InsertTextWithRecord(b.cursor, time.Now().Format(goLayout))
				b.EndTransaction()
			},
			tcell.KeyCtrlQ: func(e *Editor, s tcell.Screen) {
				for _, b := range e.buffers {
					if b.isModified { e.promptMode = true; e.promptType = "quit"; return }
				}
				s.Fini(); os.Exit(0)
			},
			tcell.KeyCtrlT: func(e *Editor, s tcell.Screen) { e.getActive().clearSelection(); e.toggleConfigBuffer() },
				 tcell.KeyCtrlS: func(e *Editor, s tcell.Screen) { e.saveActiveFile(s) },
				 tcell.KeyCtrlO: func(e *Editor, s tcell.Screen) { e.openFile(s) },
				 tcell.KeyF12:   func(e *Editor, s tcell.Screen) { e.saveAsFile(s) },
				 tcell.KeyCtrlP: func(e *Editor, s tcell.Screen) { e.paletteActive = !e.paletteActive; e.paletteCursor = 0; e.ctxMenuActive = false; e.encodeMenuActive = false },
				 tcell.KeyCtrlA: func(e *Editor, s tcell.Screen) { e.getActive().selectAll() },
				 tcell.KeyCtrlZ: func(e *Editor, s tcell.Screen) { e.getActive().Undo() },
				 tcell.KeyCtrlY: func(e *Editor, s tcell.Screen) { e.getActive().Redo() },
				 tcell.KeyCtrlC: func(e *Editor, s tcell.Screen) { text := e.getActive().getSelectedText(); if text != "" { _ = clipboard.WriteAll(text) } },
			tcell.KeyCtrlX: func(e *Editor, s tcell.Screen) {
				b := e.getActive(); if b.isReadOnly { return }
				text := b.getSelectedText()
				if text != "" { _ = clipboard.WriteAll(text); b.BeginTransaction(); b.DeleteSelection(); b.EndTransaction() }
			},
			tcell.KeyCtrlV: func(e *Editor, s tcell.Screen) {
				b := e.getActive(); if b.isReadOnly { return }
				text, err := clipboard.ReadAll()
					 if err == nil && text != "" {
						 b.BeginTransaction(); b.DeleteSelection()
						 text = strings.ReplaceAll(text, "\r\n", "\n")
						 b.InsertTextWithRecord(b.cursor, text)
						 b.EndTransaction()
					 }
				 },
				 tcell.KeyCtrlN: func(e *Editor, s tcell.Screen) { e.buffers = append(e.buffers, NewBuffer()); e.activeBuffer = len(e.buffers) - 1 },
				 tcell.KeyCtrlW: func(e *Editor, s tcell.Screen) {
					 b := e.getActive()
					 if b.isModified { e.promptMode = true; e.promptType = "close"; e.targetCloseBuffer = e.activeBuffer; return }
					 if len(e.buffers) <= 1 { s.Fini(); os.Exit(0) }
					 if b.filePath != "" { e.fileWatcher.Remove(b.filePath) }
					 e.buffers = append(e.buffers[:e.activeBuffer], e.buffers[e.activeBuffer+1:]...)
					 if e.activeBuffer >= len(e.buffers) { e.activeBuffer = len(e.buffers) - 1 }
					 e.needsFullRefresh = true
				 },
				 tcell.KeyCtrlBackslash: func(e *Editor, s tcell.Screen) { e.activeBuffer = (e.activeBuffer + 1) % len(e.buffers) },
				 tcell.KeyCtrlF: func(e *Editor, s tcell.Screen) {
					 b := e.getActive(); b.clearSelection(); b.searchMode = true; b.isReplace = false; b.replaceStep = 0
					 b.searchQuery = []rune{}; b.replaceQuery = []rune{}; b.matches = []MatchInfo{}; b.matchIdx = -1; b.inputCX = 0; b.isInputSelect = false
				 },
				 tcell.KeyCtrlR: func(e *Editor, s tcell.Screen) {
					 b := e.getActive(); b.clearSelection(); b.searchMode = true; b.isReplace = true; b.replaceStep = 1
					 b.searchQuery = []rune{}; b.replaceQuery = []rune{}; b.matches = []MatchInfo{}; b.matchIdx = -1; b.inputCX = 0; b.isInputSelect = false
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
	if err != nil { absPath = filePath }

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
	
	if len(e.buffers) == 1 && e.buffers[0].filePath == "" && !e.buffers[0].isModified {
		e.buffers[0] = b
		e.activeBuffer = 0
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
	if path == "" { return }
	strData, _, err := readFileDetectEncoding(path)
	if err != nil { return }

	b := NewBuffer()
	b.filePath = path
	b.isConfig = true
	b.isReadOnly = isReadOnly
	e.fileWatcher.Add(path)
	b.setLinesFromText(strData)
	b.savedContent = b.getContent()
	b.savedTotalChars = b.totalChars

	if len(e.buffers) == 1 && e.buffers[0].filePath == "" && !e.buffers[0].isModified {
		e.buffers[0] = b
		e.activeBuffer = 0
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
		case "-R", "--readonly": currentRO = true
		case "-e", "--edit":     currentRO = false
		case "-c", "--config":   actions = append(actions, StartupAction{Type: "config", ReadOnly: currentRO})
		case "-o", "--open":     actions = append(actions, StartupAction{Type: "picker", ReadOnly: currentRO})
		case "-n", "--new":      actions = append(actions, StartupAction{Type: "new", ReadOnly: currentRO})
		case "-v", "--version":
			fmt.Println("jigedit v1.0 - The Terminal Editor")
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
					if ch == 'R' { currentRO = true }
					if ch == 'e' { currentRO = false }
					if ch == 'c' { actions = append(actions, StartupAction{Type: "config", ReadOnly: currentRO}) }
					if ch == 'o' { actions = append(actions, StartupAction{Type: "picker", ReadOnly: currentRO}) }
				}
			} else {
				actions = append(actions, StartupAction{Type: "file", Path: arg, ReadOnly: currentRO})
			}
		}
	}

	s, err := tcell.NewScreen()
	if err != nil { log.Fatalf("%v", err) }
	if err := s.Init(); err != nil { log.Fatalf("%v", err) }
	defer s.Fini()

	s.EnableMouse(tcell.MouseMotionEvents)
	globalScreenHandle = &s
	editor := NewEditor()
	defer editor.fileWatcher.Close()

	// 💡 파싱된 큐(Queue) 순차 실행 엔진
	if len(actions) > 0 {
		w, _ := s.Size()
		// 파일 픽커가 뜨기 전에 에디터 껍데기를 먼저 그려줍니다.
		editor.draw(s, editor.getActive().generateVisualLines(w, editor.cfg))

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
				if len(editor.buffers) == 1 && editor.buffers[0].filePath == "" && !editor.buffers[0].isModified {
					editor.buffers[0] = b
					editor.activeBuffer = 0
				} else {
					editor.buffers = append(editor.buffers, b)
					editor.activeBuffer = len(editor.buffers) - 1
				}
			}
		}
		editor.needsFullRefresh = true
	}

			var cachedVLines []VisualLine
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
					cachedVLines = b.generateVisualLines(w, editor.cfg)
					needsLayout = false
				}

				if snapToCursor && !editor.paletteActive && !editor.ctxMenuActive && !editor.encodeMenuActive && !currentScreen.HasPendingEvent() {
					b.scrollToCursorV(editor.cfg, h-editor.tabHeight-1)
					textMaxWidth := w - b.getLineNumWidth(editor.cfg)
					if !editor.cfg.LineWrapping || b.isLineUnwrapped(b.cursor.L, editor.cfg) { b.scrollToCursorH(textMaxWidth, editor.cfg) } else { b.hOffset = 0 }
					snapToCursor = false
				}

				if !currentScreen.HasPendingEvent() { editor.draw(currentScreen, cachedVLines) }

				ev := currentScreen.PollEvent()
				switch ev := ev.(type) {
					case *tcell.EventResize: currentScreen.Sync(); needsLayout = true; snapToCursor = true
					case *tcell.EventInterrupt:
						filePath, ok := ev.Data().(string)
						if ok {
							// 💡 백그라운드 고루틴이 탭 상태 변경을 감지했을 때 화면 강제 갱신
							if filePath == "refresh_mod_state" {
								editor.needsFullRefresh = true
								needsLayout = true
								continue
							}

							
							for i, buf := range editor.buffers {
								if buf.filePath == filePath {
									strData, _, err := readFileDetectEncoding(filePath)
									if err == nil && strData != buf.savedContent {
										if buf.isModified {
											editor.promptMode = true; editor.promptType = "external_change"; editor.targetCloseBuffer = i; needsLayout = true
										} else {
											if buf.reloadFromDisk() {
												if buf.isConfig {
													var newCfg Config; if err := json.Unmarshal([]byte(buf.savedContent), &newCfg); err == nil { editor.cfg = newCfg }
												}
												needsLayout = true; snapToCursor = true
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
						if isWheel { isNewPress = false; isDrag = (lastMouseButtons&tcell.Button1 != 0)
						} else {
							oldButtons := lastMouseButtons; lastMouseButtons = buttons
							isNewPress = (buttons&tcell.Button1 != 0) && (oldButtons&tcell.Button1 == 0)
							isDrag = (buttons&tcell.Button1 != 0) && !isNewPress
						}

						if (buttons&tcell.Button3 != 0 || buttons&tcell.Button2 != 0) && !editor.paletteActive {
							editor.ctxMenuActive = true; editor.ctxMenuX = mx; editor.ctxMenuY = my; editor.ctxMenuCursor = 0
							editor.paletteActive = false; editor.encodeMenuActive = false; needsLayout = true; continue
						}

						if editor.ctxMenuActive {
							if mx >= editor.ctxMenuX && mx < editor.ctxMenuX+editor.ctxMenuW && my >= editor.ctxMenuY && my < editor.ctxMenuY+editor.ctxMenuH {
								clickIdx := my - editor.ctxMenuY - 1
								if clickIdx >= 0 && clickIdx < len(editor.ctxMenuItems) {
									if editor.ctxMenuCursor != clickIdx { editor.ctxMenuCursor = clickIdx; needsLayout = true }
									if isNewPress {
										action := editor.ctxMenuItems[editor.ctxMenuCursor].Action; editor.ctxMenuActive = false
										if action != nil { action(editor, currentScreen) }; needsLayout = true
									}
								}
							} else if isNewPress { editor.ctxMenuActive = false; needsLayout = true }
							continue
						}

						if editor.encodeMenuActive {
							if mx >= editor.encodeMenuX && mx < editor.encodeMenuX+editor.encodeMenuW && my >= editor.encodeMenuY && my < editor.encodeMenuY+editor.encodeMenuH {
								clickIdx := my - editor.encodeMenuY - 1
								if clickIdx >= 0 && clickIdx < len(editor.encodeMenuItems) {
									if editor.encodeMenuCursor != clickIdx { editor.encodeMenuCursor = clickIdx; needsLayout = true }
									if isNewPress {
										action := editor.encodeMenuItems[editor.encodeMenuCursor].Action; editor.encodeMenuActive = false
										if action != nil { action(editor, currentScreen) }; needsLayout = true
									}
								}
							} else if isNewPress { editor.encodeMenuActive = false; needsLayout = true }
							continue
						}

						if !b.searchMode && !b.gotoMode && my == h-1 {
							if !b.isConfig && mx >= b.encodeBtnX1 && mx <= b.encodeBtnX2 && isNewPress {
								editor.encodeMenuActive = true; editor.encodeMenuX = mx; editor.encodeMenuY = h - 4; editor.encodeMenuCursor = 0; needsLayout = true; continue
							}
						}

						if (b.searchMode || b.gotoMode) && (my == h-1 || (b.isInputSelect && isDrag)) {
							if my == h-1 && mx >= b.closeBtnStartX && mx <= b.closeBtnEndX && isNewPress {
								b.searchMode = false; b.gotoMode = false; b.isReplace = false; b.isInputSelect = false; needsLayout = true; continue
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

								if mx >= b.chkRegexX1 && mx <= b.chkRegexX2 { b.searchRegex = !b.searchRegex; doSearchJump(); continue }
								if mx >= b.chkCaseX1 && mx <= b.chkCaseX2 { b.searchCase = !b.searchCase; doSearchJump(); continue }
								if mx >= b.chkWordX1 && mx <= b.chkWordX2 { b.searchWord = !b.searchWord; doSearchJump(); continue }
							}
							var prefix string
							var targetStr *[]rune
							if b.gotoMode { prefix = " [Go To] Line,Col: "; targetStr = &b.gotoInput
							} else if b.isReplace {
								if b.replaceStep == 1 { prefix = " [Replace] Find: "; targetStr = &b.searchQuery
								} else if b.replaceStep == 2 { prefix = " [Replace] Find: " + string(b.searchQuery) + "  ➔ Replace: "; targetStr = &b.replaceQuery }
							} else { prefix = " [Find] Search: "; targetStr = &b.searchQuery }

							if targetStr != nil {
								prefixW := runewidth.StringWidth(prefix)
								idx := 0
								if mx >= prefixW {
									currW := prefixW
									for i, r := range *targetStr {
										rw := runewidth.RuneWidth(r); if mx >= currW && mx < currW+rw { idx = i; break }
										currW += rw; idx = i + 1
									}
								}
								if isNewPress && my == h-1 { b.inputCX = idx; b.isInputSelect = true; b.inputSelStart = idx; b.inputSelEnd = idx
								} else if isDrag && b.isInputSelect { b.inputCX = idx; b.inputSelEnd = idx; b.isInputSelect = true }
								needsLayout = true
							}
							if my == h-1 || b.isInputSelect { continue }
						}

						if editor.paletteActive {
							if mx >= editor.paletteX && mx < editor.paletteX+editor.paletteW && my >= editor.paletteY && my < editor.paletteY+editor.paletteH {
								clickIdx := my - editor.paletteY - 1; visibleItems := editor.paletteH - 2
								startIdx := editor.paletteCursor - visibleItems/2; if startIdx < 0 { startIdx = 0 }
								if startIdx+visibleItems > len(editor.paletteItems) { startIdx = len(editor.paletteItems) - visibleItems; if startIdx < 0 { startIdx = 0 } }
								targetItemIdx := startIdx + clickIdx
								if targetItemIdx >= 0 && targetItemIdx < len(editor.paletteItems) {
									if editor.paletteCursor != targetItemIdx { editor.paletteCursor = targetItemIdx; needsLayout = true }
									if isNewPress {
										action := editor.paletteItems[editor.paletteCursor].Action; editor.paletteActive = false
										if action != nil { action(editor, currentScreen) }; needsLayout = true
									}
								}
							} else if isNewPress { editor.paletteActive = false; needsLayout = true }
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
							if buttons&tcell.WheelUp != 0 { editor.activeBuffer = (editor.activeBuffer - 1 + len(editor.buffers)) % len(editor.buffers); editor.needsFullRefresh = true; needsLayout = true; continue }
							if buttons&tcell.WheelDown != 0 { editor.activeBuffer = (editor.activeBuffer + 1) % len(editor.buffers); editor.needsFullRefresh = true; needsLayout = true; continue }
						}
						outOfBounds := my < editor.tabHeight || my >= h-1
						if buttons&tcell.WheelUp != 0 {
							totalV := b.getVisualLineCount(editor.cfg)
							if !outOfBounds && b.vOffsetIdx > 0 && b.vOffsetIdx < totalV { b.vOffsetIdx-- }
							continue
						}
						if buttons&tcell.WheelDown != 0 {
							totalV := b.getVisualLineCount(editor.cfg)
							if !outOfBounds && b.vOffsetIdx+1 < totalV { b.vOffsetIdx++ }
							continue
						}

						if buttons&tcell.Button1 != 0 {
							loc := b.screenToMemoryPosV(cachedVLines, mx, my, editor.tabHeight, editor.cfg)
							if isNewPress && outOfBounds { continue }

							if isNewPress {
								now := time.Now()
								if now.Sub(lastClickTime) < 400*time.Millisecond && mx == lastClickX && my == lastClickY { clickCount++ } else { clickCount = 1 }
								lastClickTime = now; lastClickX, lastClickY = mx, my

								if clickCount == 1 {
									if !b.isSelecting { b.isSelecting = true; b.selection.Start = loc }
									b.selection.End = loc; b.cursor = loc
								} else if clickCount == 2 {
									b.cursor = loc; b.selectWordAtCursor()
								} else if clickCount >= 3 {
									b.cursor = loc; b.selectLineAtCursor(); clickCount = 0
								}
							} else {
								// 💡 마우스가 완벽히 멈춰있을 때는 더블/트리플 클릭 선택 영역을 취소하지 않음!
								if mx != lastClickX || my != lastClickY {
									b.selection.End = loc; b.cursor = loc
								}
								if my < editor.tabHeight && b.vOffsetIdx > 0 { b.vOffsetIdx-- } else if my >= h-1 && b.vOffsetIdx+1 < b.getVisualLineCount(editor.cfg) { b.vOffsetIdx++ }
							}
							snapToCursor = true
						} else {
							if b.isSelecting {
								b.isSelecting = false
								if b.selection.Start == b.selection.End { b.clearSelection() }
							}
						}
						case *tcell.EventKey:
							isCtrl := (ev.Modifiers() & tcell.ModCtrl) != 0
							isShift := (ev.Modifiers() & tcell.ModShift) != 0
							isAlt := (ev.Modifiers() & tcell.ModAlt) != 0
							snapToCursor = true

							// 💡 탭 좌우 이동 시 화면 렌더링 캐시 동기화

							// 💡 탭 좌우 이동 시 화면 렌더링 캐시 동기화
						if isAlt && ev.Rune() == ',' { editor.activeBuffer = (editor.activeBuffer - 1 + len(editor.buffers)) % len(editor.buffers); editor.needsFullRefresh = true; needsLayout = true; continue }
if isAlt && ev.Rune() == '.' { editor.activeBuffer = (editor.activeBuffer + 1) % len(editor.buffers); editor.needsFullRefresh = true; needsLayout = true; continue }

							if editor.promptMode {
								// 💡 Alert 모드일 때는 y/n이 아니라 Enter나 Esc로 단순히 닫음
								if editor.promptType == "alert" {
									if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyEnter { 
										editor.promptMode = false
										needsLayout = true 
									}
									continue
								}

								if ev.Key() == tcell.KeyEscape || ev.Rune() == 'n' || ev.Rune() == 'N' { editor.promptMode = false; needsLayout = true
								} else if ev.Rune() == 'y' || ev.Rune() == 'Y' {
									editor.promptMode = false

									if editor.promptType == "quit" { currentScreen.Fini(); os.Exit(0)
									} else if editor.promptType == "close" {
										target := editor.targetCloseBuffer
										if len(editor.buffers) <= 1 { currentScreen.Fini(); os.Exit(0) }
										bufToClose := editor.buffers[target]; if bufToClose.filePath != "" { editor.fileWatcher.Remove(bufToClose.filePath) }
										editor.buffers = append(editor.buffers[:target], editor.buffers[target+1:]...)
										if editor.activeBuffer == target { if editor.activeBuffer >= len(editor.buffers) { editor.activeBuffer = len(editor.buffers) - 1 }
										} else if editor.activeBuffer > target { editor.activeBuffer-- }
										editor.needsFullRefresh = true; needsLayout = true // ✅ 수정됨
									} else if editor.promptType == "reset_config" {
										defaultCfg := DefaultConfig(); data, _ := json.MarshalIndent(defaultCfg, "", "    "); _ = ioutil.WriteFile(getConfigPath(), data, 0644); editor.cfg = defaultCfg
											for _, buf := range editor.buffers { if buf.isConfig { buf.isModified = false; buf.reloadFromDisk() } }
											editor.needsFullRefresh = true; needsLayout = true // ✅ 수정됨
									} else if editor.promptType == "reopen" { editor.getActive().reopenWithEncoding(editor.targetEncoding); editor.needsFullRefresh = true; needsLayout = true // ✅ 수정됨
									} else if editor.promptType == "close_config" {
										target := editor.targetCloseBuffer; bufToClose := editor.buffers[target]; if bufToClose.filePath != "" { editor.fileWatcher.Remove(bufToClose.filePath) }
										editor.buffers = append(editor.buffers[:target], editor.buffers[target+1:]...); editor.activeBuffer = 0; editor.needsFullRefresh = true; needsLayout = true // ✅ 수정됨
									} else if editor.promptType == "external_change" {
										target := editor.targetCloseBuffer; bufToReload := editor.buffers[target]; bufToReload.isModified = false
										if bufToReload.reloadFromDisk() { if bufToReload.isConfig { var newCfg Config; if err := json.Unmarshal([]byte(bufToReload.savedContent), &newCfg); err == nil { editor.cfg = newCfg } } }
										editor.needsFullRefresh = true; needsLayout = true; snapToCursor = true // ✅ 수정됨
									}
								}
								continue
							}

							if editor.paletteActive {
								switch ev.Key() {
									case tcell.KeyEscape: editor.paletteActive = false
									case tcell.KeyUp: editor.paletteCursor--; if editor.paletteCursor < 0 { editor.paletteCursor = len(editor.paletteItems) - 1 }
									case tcell.KeyDown: editor.paletteCursor++; if editor.paletteCursor >= len(editor.paletteItems) { editor.paletteCursor = 0 }
									case tcell.KeyEnter: action := editor.paletteItems[editor.paletteCursor].Action; editor.paletteActive = false; if action != nil { action(editor, currentScreen) }
								}
								needsLayout = true; continue
							}

							if editor.ctxMenuActive {
								switch ev.Key() {
									case tcell.KeyEscape: editor.ctxMenuActive = false
									case tcell.KeyUp: editor.ctxMenuCursor--; if editor.ctxMenuCursor < 0 { editor.ctxMenuCursor = len(editor.ctxMenuItems) - 1 }
									case tcell.KeyDown: editor.ctxMenuCursor++; if editor.ctxMenuCursor >= len(editor.ctxMenuItems) { editor.ctxMenuCursor = 0 }
									case tcell.KeyEnter: action := editor.ctxMenuItems[editor.ctxMenuCursor].Action; editor.ctxMenuActive = false; if action != nil { action(editor, currentScreen) }
								}
								needsLayout = true; continue
							}

							if editor.encodeMenuActive {
								switch ev.Key() {
									case tcell.KeyEscape: editor.encodeMenuActive = false
									case tcell.KeyUp: editor.encodeMenuCursor--; if editor.encodeMenuCursor < 0 { editor.encodeMenuCursor = len(editor.encodeMenuItems) - 1 }
									case tcell.KeyDown: editor.encodeMenuCursor++; if editor.encodeMenuCursor >= len(editor.encodeMenuItems) { editor.encodeMenuCursor = 0 }
									case tcell.KeyEnter: action := editor.encodeMenuItems[editor.encodeMenuCursor].Action; editor.encodeMenuActive = false; if action != nil { action(editor, currentScreen) }
								}
								needsLayout = true; continue
							}

							if b.searchMode || b.gotoMode {
								if ev.Key() == tcell.KeyEscape {
									b.searchMode = false; b.isReplace = false; b.replaceStep = 0; b.gotoMode = false
									b.clearSelection(); b.isInputSelect = false; needsLayout = true; continue
								}

								var targetStr *[]rune
								if b.gotoMode { targetStr = &b.gotoInput
								} else if b.isReplace && b.replaceStep == 1 { targetStr = &b.searchQuery
								} else if b.isReplace && b.replaceStep == 2 { targetStr = &b.replaceQuery
								} else if !b.isReplace { targetStr = &b.searchQuery }

								if targetStr != nil {
									hasSel := b.isInputSelect && b.inputSelStart != b.inputSelEnd
									selStart, selEnd := b.inputSelStart, b.inputSelEnd
									if selStart > selEnd { selStart, selEnd = selEnd, selStart }

									deleteInputSel := func() {
										if hasSel {
											*targetStr = append((*targetStr)[:selStart], (*targetStr)[selEnd:]...)
											b.inputCX = selStart; b.isInputSelect = false; b.inputSelStart = 0; b.inputSelEnd = 0
										}
									}

									if ev.Key() == tcell.KeyCtrlA { b.isInputSelect = true; b.inputSelStart = 0; b.inputSelEnd = len(*targetStr); b.inputCX = len(*targetStr); needsLayout = true; continue }
									if ev.Key() == tcell.KeyCtrlC && hasSel { clipboard.WriteAll(string((*targetStr)[selStart:selEnd])); continue }
									if ev.Key() == tcell.KeyCtrlX && hasSel {
										clipboard.WriteAll(string((*targetStr)[selStart:selEnd])); deleteInputSel()
										if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }
										needsLayout = true; continue
									}
									if ev.Key() == tcell.KeyCtrlV {
										text, err := clipboard.ReadAll()
										if err == nil && text != "" {
											deleteInputSel()
											text = strings.ReplaceAll(text, "\r\n", " "); text = strings.ReplaceAll(text, "\n", " "); text = strings.ReplaceAll(text, "\r", "")
											runes := []rune(text)
											*targetStr = append((*targetStr)[:b.inputCX], append(runes, (*targetStr)[b.inputCX:]...)...)
											b.inputCX += len(runes)
											if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }
											needsLayout = true
										}
										continue
									}

									if ev.Key() == tcell.KeyLeft {
										if !isShift && hasSel { b.isInputSelect = false; b.inputCX = selStart
										} else {
											if b.inputCX > 0 { b.inputCX-- }
											if isShift { if !b.isInputSelect { b.isInputSelect = true; b.inputSelStart = b.inputCX + 1 }; b.inputSelEnd = b.inputCX
											} else { b.isInputSelect = false }
										}
										needsLayout = true; continue
									}
									if ev.Key() == tcell.KeyRight {
										if !isShift && hasSel { b.isInputSelect = false; b.inputCX = selEnd
										} else {
											if b.inputCX < len(*targetStr) { b.inputCX++ }
											if isShift { if !b.isInputSelect { b.isInputSelect = true; b.inputSelStart = b.inputCX - 1 }; b.inputSelEnd = b.inputCX
											} else { b.isInputSelect = false }
										}
										needsLayout = true; continue
									}
									if ev.Key() == tcell.KeyHome {
										if !isShift && hasSel { b.isInputSelect = false }
										if isShift { if !b.isInputSelect { b.isInputSelect = true; b.inputSelStart = b.inputCX }; b.inputCX = 0; b.inputSelEnd = 0
										} else { b.inputCX = 0; b.isInputSelect = false }
										needsLayout = true; continue
									}
									if ev.Key() == tcell.KeyEnd {
										if !isShift && hasSel { b.isInputSelect = false }
										if isShift { if !b.isInputSelect { b.isInputSelect = true; b.inputSelStart = b.inputCX }; b.inputCX = len(*targetStr); b.inputSelEnd = b.inputCX
										} else { b.inputCX = len(*targetStr); b.isInputSelect = false }
										needsLayout = true; continue
									}

									if ev.Key() == tcell.KeyBackspace || ev.Key() == tcell.KeyBackspace2 {
										if hasSel {
											deleteInputSel(); if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }; needsLayout = true
										} else if b.inputCX > 0 {
											*targetStr = append((*targetStr)[:b.inputCX-1], (*targetStr)[b.inputCX:]...); b.inputCX--
											if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }; needsLayout = true
										} else if b.inputCX == 0 && b.isReplace && b.replaceStep == 2 {
											b.replaceStep = 1; b.inputCX = len(b.searchQuery); b.isInputSelect = false; needsLayout = true
										}
										continue
									}
									if ev.Key() == tcell.KeyDelete {
										if hasSel { deleteInputSel(); if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }; needsLayout = true
										} else if b.inputCX < len(*targetStr) {
											*targetStr = append((*targetStr)[:b.inputCX], (*targetStr)[b.inputCX+1:]...)
											if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }; needsLayout = true
										}
										continue
									}
									if ev.Key() == tcell.KeyRune && ev.Rune() != 0 {
										deleteInputSel()
										*targetStr = append((*targetStr)[:b.inputCX], append([]rune{ev.Rune()}, (*targetStr)[b.inputCX:]...)...); b.inputCX++
										if b.searchMode && (!b.isReplace || b.replaceStep == 1) { b.matches = nil; b.matchIdx = -1 }; needsLayout = true
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
											b.searchMode = false; b.isReplace = false; b.replaceStep = 0
											b.clearSelection(); needsLayout = true
										}
										continue
									}

									if ev.Key() == tcell.KeyUp { if len(b.matches) > 0 { b.matchIdx = (b.matchIdx - 1 + len(b.matches)) % len(b.matches); b.jumpToMatch(); snapToCursor = true }; continue }
									if ev.Key() == tcell.KeyDown { if len(b.matches) > 0 { b.matchIdx = (b.matchIdx + 1) % len(b.matches); b.jumpToMatch(); snapToCursor = true }; continue }
									if ev.Key() == tcell.KeyEnter {
										if b.searchMode && (!b.isReplace || b.replaceStep == 1) && b.matchIdx == -1 {
											b.findAllMatches(editor.cfg.OverlapSearch)
											if len(b.matches) > 0 {
												b.matchIdx = b.findInitialMatchIdx(b.cursor, isShift)
												b.jumpToMatch()
												snapToCursor = true
											}
											if b.isReplace && b.replaceStep == 1 {
												b.replaceStep = 2; b.inputCX = len(b.replaceQuery); b.isInputSelect = false
											}
										} else if isShift {
											if len(b.matches) > 0 { b.matchIdx = (b.matchIdx - 1 + len(b.matches)) % len(b.matches); b.jumpToMatch(); snapToCursor = true }
										} else {
											if b.isReplace {
												if b.replaceStep == 1 { b.replaceStep = 2; b.inputCX = len(b.replaceQuery); b.isInputSelect = false; if len(b.matches) > 0 { b.matchIdx = b.findInitialMatchIdx(b.cursor, false); b.jumpToMatch(); snapToCursor = true }
												} else if b.replaceStep == 2 { b.replaceStep = 3; b.findAllMatches(editor.cfg.OverlapSearch); if len(b.matches) > 0 { b.matchIdx = b.findInitialMatchIdx(b.cursor, false); b.jumpToMatch(); snapToCursor = true }
												} else if b.replaceStep == 3 { b.replaceCurrent(editor.cfg.OverlapSearch); needsLayout = true; snapToCursor = true }
											} else { if len(b.matches) > 0 { b.matchIdx = (b.matchIdx + 1) % len(b.matches); b.jumpToMatch(); snapToCursor = true } }
										}
										continue
									}
								}

								if b.gotoMode {
									if ev.Key() == tcell.KeyEnter {
										inputStr := string(b.gotoInput); parts := strings.Split(inputStr, ",")
										lineNum, colNum := 0, 0
										fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &lineNum)
										if len(parts) > 1 { fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &colNum) }
										if lineNum > 0 {
											lineNum--
											if lineNum >= len(b.lines) { lineNum = len(b.lines) - 1 }
											b.cursor.L = lineNum; b.cursor.C = colNum - 1
											if b.cursor.C < 0 { b.cursor.C = 0 }
											if b.cursor.C > len(b.lines[b.cursor.L]) { b.cursor.C = len(b.lines[b.cursor.L]) }
										}
										b.gotoMode = false; snapToCursor = true; needsLayout = true; continue
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
									b.EndTransaction(); needsLayout = true
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
									b.EndTransaction(); needsLayout = true
								}
								continue
							}
							if ev.Key() == tcell.KeyLeft || ev.Key() == tcell.KeyRight || ev.Key() == tcell.KeyUp || ev.Key() == tcell.KeyDown || ev.Key() == tcell.KeyHome || ev.Key() == tcell.KeyEnd || ev.Key() == tcell.KeyPgUp || ev.Key() == tcell.KeyPgDn {
								if !isShift { b.clearSelection() } else { if !b.isSelecting { b.isSelecting = true; b.selection.Start = b.cursor } }
							}

							if action, exists := ActionMap[ev.Key()]; exists { action(editor, currentScreen); needsLayout = true; continue }

							switch ev.Key() {
								case tcell.KeyTab, tcell.KeyBacktab:
									b.BeginTransaction()
									s, e := b.getSelectionRange()
									hasSel := b.HasSelection()
									if !hasSel { s = b.cursor; e = b.cursor }
									oldCursor := b.cursor

									if isShift || ev.Key() == tcell.KeyBacktab {
										for r := s.L; r <= e.L; r++ {
											line := b.lines[r]
											if len(line) > 0 {
												removeCount := 0
												if line[0] == '\t' { removeCount = 1 } else {
													for removeCount < len(line) && removeCount < editor.cfg.TabSize && line[removeCount] == ' ' { removeCount++ }
												}
												if removeCount > 0 {
													b.DeleteTextWithRecord(Loc{r, 0}, Loc{r, removeCount})
													if r == oldCursor.L { oldCursor.C -= removeCount; if oldCursor.C < 0 { oldCursor.C = 0 } }
													if hasSel {
														if r == s.L { s.C -= removeCount; if s.C < 0 { s.C = 0 } }
														if r == e.L { e.C -= removeCount; if e.C < 0 { e.C = 0 } }
													}
												}
											}
										}
									} else {
										spaces := strings.Repeat(" ", editor.cfg.TabSize)
										if hasSel && s.L != e.L { // 💡 다중 줄 선택 시 전체 줄 들여쓰기
											for r := e.L; r >= s.L; r-- {
												b.InsertTextWithRecord(Loc{r, 0}, spaces)
												if r == oldCursor.L { oldCursor.C += editor.cfg.TabSize }
												if r == s.L { s.C += editor.cfg.TabSize }
												if r == e.L { e.C += editor.cfg.TabSize }
											}
										} else { 
											// 💡 단일 줄 내에서 글자를 드래그한 상태면 지우고 탭 삽입
											if hasSel { b.DeleteSelection(); oldCursor = b.cursor; hasSel = false }
											b.InsertTextWithRecord(oldCursor, spaces); oldCursor.C += editor.cfg.TabSize 
										}
									}
									b.cursor = oldCursor
									if hasSel { b.selection.Start = s; b.selection.End = e }
									b.EndTransaction(); needsLayout = true

								case tcell.KeyLeft:
									if isCtrl { b.moveWordLeft() } else { if b.cursor.C > 0 { b.cursor.C-- } else if b.cursor.L > 0 { b.cursor.L--; b.cursor.C = len(b.lines[b.cursor.L]) } }
								case tcell.KeyRight:
									if isCtrl { b.moveWordRight() } else { if b.cursor.C < len(b.lines[b.cursor.L]) { b.cursor.C++ } else if b.cursor.L < len(b.lines)-1 { b.cursor.L++; b.cursor.C = 0 } }
								case tcell.KeyUp:
									if isCtrl { b.moveParagraphUp() } else {
										currentVIdx := b.getVCursorIdx(editor.cfg)
										if currentVIdx > 0 {
											targetVL := b.getVisualLine(currentVIdx-1, editor.cfg); currentVL := b.getVisualLine(currentVIdx, editor.cfg)
											offset := b.cursor.C - currentVL.startCX
											b.cursor.L = targetVL.lineIdx; b.cursor.C = targetVL.startCX + offset
											if b.cursor.C > targetVL.endCX { b.cursor.C = targetVL.endCX }
										} else if currentVIdx == 0 { b.cursor.C = 0 }
									}
								case tcell.KeyDown:
									if isCtrl { b.moveParagraphDown() } else {
										currentVIdx := b.getVCursorIdx(editor.cfg); totalV := b.getVisualLineCount(editor.cfg)
										if currentVIdx != -1 && currentVIdx+1 < totalV {
											targetVL := b.getVisualLine(currentVIdx+1, editor.cfg); currentVL := b.getVisualLine(currentVIdx, editor.cfg)
											offset := b.cursor.C - currentVL.startCX
											b.cursor.L = targetVL.lineIdx; b.cursor.C = targetVL.startCX + offset
											if b.cursor.C > targetVL.endCX { b.cursor.C = targetVL.endCX }
										} else if currentVIdx == totalV-1 { currentVL := b.getVisualLine(currentVIdx, editor.cfg); b.cursor.C = currentVL.endCX }
									}
								case tcell.KeyPgUp:
									newRow := b.cursor.L - (h - 2); if newRow < 0 { newRow = 0 }; b.cursor.L = newRow; b.cursor.C = 0
								case tcell.KeyPgDn:
									newRow := b.cursor.L + (h - 2); if newRow >= len(b.lines) { newRow = len(b.lines) - 1 }; if newRow < 0 { newRow = 0 }; b.cursor.L = newRow; b.cursor.C = 0
								case tcell.KeyHome:
									if isCtrl {
										b.cursor = Loc{0, 0}
									} else {
										currentVIdx := b.getVCursorIdx(editor.cfg); currentVL := b.getVisualLine(currentVIdx, editor.cfg); b.cursor.C = currentVL.startCX
									}
								case tcell.KeyEnd:
									if isCtrl {
										lastLineIdx := len(b.lines) - 1
										if lastLineIdx < 0 { lastLineIdx = 0 }
										b.cursor = Loc{lastLineIdx, len(b.lines[lastLineIdx])}
									} else {
										currentVIdx := b.getVCursorIdx(editor.cfg); currentVL := b.getVisualLine(currentVIdx, editor.cfg); b.cursor.C = currentVL.endCX
									}
								case tcell.KeyEnter:
									b.BeginTransaction(); b.DeleteSelection()
									indentStr := ""
									if editor.cfg.AutoIndent {
										line := b.lines[b.cursor.L]
										for i := 0; i < b.cursor.C && i < len(line); i++ {
											if line[i] == ' ' || line[i] == '\t' { indentStr += string(line[i]) } else { break }
										}
									}
									b.InsertTextWithRecord(b.cursor, "\n"+indentStr)
									b.EndTransaction(); needsLayout = true

								case tcell.KeyBackspace2, tcell.KeyBackspace:
									b.BeginTransaction()
									if !b.DeleteSelection() {
										if b.cursor.C > 0 {
											startX := b.cursor.C
											if isCtrl || isAlt {
												line := b.lines[b.cursor.L]
												for startX > 0 && unicode.IsSpace(line[startX-1]) { startX-- }
												if startX > 0 {
													isAlpha := isWordChar(line[startX-1])
													for startX > 0 && !unicode.IsSpace(line[startX-1]) && isWordChar(line[startX-1]) == isAlpha { startX-- }
												}
											} else { startX = b.cursor.C - 1 }
											b.DeleteTextWithRecord(Loc{b.cursor.L, startX}, Loc{b.cursor.L, b.cursor.C})
										} else if b.cursor.L > 0 {
											b.DeleteTextWithRecord(Loc{b.cursor.L - 1, len(b.lines[b.cursor.L-1])}, b.cursor)
										}
									}
									b.EndTransaction(); needsLayout = true

								case tcell.KeyDelete:
									b.BeginTransaction()
									if !b.DeleteSelection() {
										lineLen := len(b.lines[b.cursor.L])
										if isCtrl || isAlt {
											if b.cursor.C < lineLen {
												endX := b.cursor.C; line := b.lines[b.cursor.L]; isAlpha := isWordChar(line[endX])
												for endX < lineLen && !unicode.IsSpace(line[endX]) && isWordChar(line[endX]) == isAlpha { endX++ }
												for endX < lineLen && unicode.IsSpace(line[endX]) { endX++ }
												b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L, endX})
											} else if b.cursor.L < len(b.lines)-1 { b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L + 1, 0}) }
										} else {
											if b.cursor.C < lineLen { b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L, b.cursor.C + 1})
											} else if b.cursor.L < len(b.lines)-1 { b.DeleteTextWithRecord(b.cursor, Loc{b.cursor.L + 1, 0}) }
										}
									}
									b.EndTransaction(); needsLayout = true

								case tcell.KeyRune:
									if ev.Rune() != 0 { b.BeginTransaction(); b.DeleteSelection(); b.InsertTextWithRecord(b.cursor, string(ev.Rune())); b.EndTransaction(); needsLayout = true }
							}
							if isShift && b.isSelecting { b.selection.End = b.cursor }
				}
			}
		}
