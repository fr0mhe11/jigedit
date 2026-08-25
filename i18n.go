package main

import "fmt"

// --- [ 다국어 카탈로그 ] ---
//
// 💡 이 파일은 main.go 단일 파일 규칙의 유일한 예외다. 문자열 카탈로그가
// 400줄 넘게 차지해서 main.go 의 섹션 배너 탐색을 방해하기 때문에 분리했다.
// 에디터 상태는 여기 두지 않는다 — 카탈로그와 조회 함수뿐이다.
//
// 사용자에게 보이는 문자열을 추가할 때는 리터럴을 그 자리에 쓰지 말고
// 여기에 키를 추가한 뒤 T("id") / Tf("id", ...) 로 참조한다.
//
// 💡 번역하지 않는 것:
//   - ctrlChordNames 와 chordString/parseChord 의 수식어 접두사
//     ("Ctrl+", "Alt+", ...) — config.json 의 단축키 직렬화 형식을 겸한다.
//   - 인코딩 이름 ("UTF-8", "CP949 (EUC-KR)") — 지역(Region) 라벨만 번역한다.
//
// 💡 초기화 사이클 없음: langEN/langKO 는 아무것도 참조하지 않는 순수 데이터
// var 초기화식이라 Go 가 어떤 init() 보다 먼저 채운다. Actions 의 init() 은
// NameID 문자열만 저장할 뿐 T 를 부르지 않는다.

type Lang string

const (
	LangEN Lang = "en"
	LangKO Lang = "ko"
)

var currentLang = LangEN

// msgs 는 항상 non-nil 인 활성 카탈로그다.
var msgs = langEN

// setLanguage 는 config 의 언어 코드를 적용한다. 모르는 값이나 빈 값은 영어로
// 떨어진다. e.applyConfig 가 유일한 호출 경로다 (CLI 도움말 출력은 예외).
func setLanguage(code string) {
	switch Lang(code) {
	case LangKO:
		currentLang, msgs = LangKO, langKO
	default:
		currentLang, msgs = LangEN, langEN
	}
}

// availableLangs 는 언어 선택 메뉴에 뜨는 순서다. 언어를 추가하려면 여기에
// 코드를 넣고, setLanguage 에 case 를 더하고, "lang.name.<코드>" 키와 카탈로그
// 하나를 만들면 된다.
var availableLangs = []Lang{LangEN, LangKO}

// T 는 메시지 ID 를 현재 언어 문자열로 바꾼다. 번역이 빠진 키는 영어로,
// 영어도 없으면 ID 자체로 떨어진다 — 메뉴 줄이 통째로 비는 것보다 낫다.
func T(id string) string {
	if s, ok := msgs[id]; ok {
		return s
	}
	if s, ok := langEN[id]; ok {
		return s
	}
	return id
}

// Tf 는 서식 문자열용 T 다. 어순이 언어마다 달라지는 문장은 문자열 연결이
// 아니라 반드시 이쪽을 써야 한다.
func Tf(id string, a ...any) string {
	return fmt.Sprintf(T(id), a...)
}

var langEN = map[string]string{
	// 언어 이름 (전환 메뉴에서 "바꿀 대상"으로 표시된다)
	"lang.name.en": "English",
	"lang.name.ko": "한국어",

	// --- 액션 (단축키 설정 메뉴의 표시 순서 = Actions 순서) ---
	"action.open":             "Open File",
	"action.save":             "Save",
	"action.save_as":          "Save As",
	"action.new_tab":          "New Tab",
	"action.close_tab":        "Close Tab",
	"action.next_tab":         "Next Tab",
	"action.prev_tab":         "Previous Tab",
	"action.cycle_tab":        "Cycle Tab",
	"action.undo":             "Undo",
	"action.redo":             "Redo",
	"action.select_all":       "Select All",
	"action.copy":             "Copy",
	"action.cut":              "Cut",
	"action.paste":            "Paste",
	"action.find":             "Find",
	"action.replace":          "Replace",
	"action.goto_line":        "Go To Line",
	"action.move_line_up":     "Move Line Up",
	"action.move_line_down":   "Move Line Down",
	"action.add_cursor_above": "Add Cursor Above",
	"action.add_cursor_below": "Add Cursor Below",
	"action.insert_time":      "Insert Time",
	"action.palette":          "Command Palette",
	"action.toggle_config":    "Edit Config File",
	"action.quit":             "Quit Editor",

	// --- 메뉴 ---
	"menu.palette.title":   " Command Palette ",
	"menu.keybindings":     "Keybindings",
	"menu.reset_config":    "Reset Config",
	"menu.toggle_readonly": "Toggle Read-Only Mode",
	"menu.set_language":    "Set Language",
	"menu.language.title":  " Select Language ",

	"menu.keybind.title":     " Keybindings ",
	"menu.keybind.reset_all": "Reset All Keybindings To Defaults",
	"menu.keybind.unbound":   "(none)",
	"menu.keybind.press":     "Press the new shortcut",
	"menu.keybind.hint":      "Esc:Cancel  Delete:Default  Backspace:Unbind",

	"menu.encode.action.title": " Select Action ",
	"menu.encode.region.title": " Select Region ",
	"menu.encode.set_save":     "Set Save Encoding...",
	"menu.encode.reopen":       "Reopen With...",
	"menu.encode.back":         "< Back",

	// --- 인코딩 지역 라벨 (encodingGroups 의 영어 이름을 키로 쓴다) ---
	"region.Unicode":             "Unicode",
	"region.Korean":              "Korean",
	"region.Japanese":            "Japanese",
	"region.Chinese Simplified":  "Chinese Simplified",
	"region.Chinese Traditional": "Chinese Traditional",
	"region.Western European":    "Western European",
	"region.Central European":    "Central European",
	"region.Cyrillic":            "Cyrillic",
	"region.Greek":               "Greek",
	"region.Turkish":             "Turkish",
	"region.Hebrew":              "Hebrew",
	"region.Arabic":              "Arabic",
	"region.Thai":                "Thai",
	"region.Baltic":              "Baltic",
	"region.Nordic":              "Nordic",

	// --- 경고 프롬프트 ---
	// 💡 [Y/n] 표시는 두 언어 모두 그대로 둔다. 승낙 키가 이벤트 루프에
	// 'y' 로 하드코딩되어 있고, 그건 이번 작업 범위가 아니다.
	"prompt.quit":            " [Warning] Some tabs have unsaved changes. Quit anyway? [Y/n]",
	"prompt.close":           " [Warning] This tab has unsaved changes. Close it? [Y/n]",
	"prompt.reset_config":    " [Warning] Reset all settings to their defaults? [Y/n]",
	"prompt.reset_keybinds":  " [Warning] Reset all keybindings to their defaults? [Y/n]",
	"prompt.reopen":          " [Warning] This tab has unsaved changes. Reopen anyway? [Y/n]",
	"prompt.close_config":    " [Warning] The config has not been saved. Close it anyway? [Y/n]",
	"prompt.external_change": " [Warning] '%s' was changed outside the editor. Overwrite with the version on disk? [Y/n]",
	"prompt.alert":           " [Alert] %s (Enter/Esc)",

	// --- 상태 표시줄 ---
	"status.config":       "CONFIG.JSON",
	"status.sel":          "%d Sel",
	"status.chars":        "%d Chars",
	"status.palette":      "Command Palette",
	"status.encode":       "Encode:",
	"status.readonly":     " | 🔒 READ-ONLY",
	"status.new_buffer":   "New Buffer",
	"status.position":     " | %s | Ln %d, Col %d | %s%s ",
	"status.tab.untitled": "Untitled",

	// --- 검색 / 바꾸기 / 줄 이동 표시줄 ---
	"search.goto.prefix":  " [Go To] Line,Col: ",
	"search.goto.hint":    "  (Enter: Go, Esc: Cancel)",
	"search.replace.find": " [Replace] Find: ",
	"search.replace.to":   "  ➔ Replace: ",
	"search.replace.hint": "] (Enter:Replace, Up:Prev, Down:Skip, Ctrl+A:All)",
	"search.find.prefix":  " [Find] Search: ",
	"search.find.hint":    "] (Enter/Down:Next, Up:Prev)",

	// --- 단축키 설정 알림 ---
	"keybind.err.palette_locked": "The command palette cannot be unbound (it is the only way back into the settings menu)",
	"keybind.err.reserved":       "%s is reserved by the editor and cannot be used",
	"keybind.err.duplicate":      "%s is already assigned to '%s'",

	// --- GUI(zenity) 대화상자 ---
	"dialog.open.title":     "Open File",
	"dialog.save_as.title":  "Save As",
	"dialog.bigfile.title":  "Large File Warning",
	"dialog.bigfile.body":   "This file is very large (%.1f MB).\nOpening it may be slow or may hang the editor. Continue?",
	"dialog.bigfile.ok":     "Yes",
	"dialog.bigfile.cancel": "No",

	// --- 오류 ---
	"err.config_path":    "could not locate the config path",
	"err.config_save":    "Failed to save settings: %v",
	"err.watcher_init":   "could not initialize the file watcher: %v",
	"err.not_regular":    "not a regular file",
	"err.encode_convert": "encoding conversion failed (save aborted): %v",
	"err.save_failed":    "Save failed: %v",
	"err.is_directory":   "'%s' is a directory.",
	"err.no_such_dir":    "Warning: directory does not exist (%s)",

	// --- CLI ---
	"cli.version":      "jigedit v1.4.0 - A Sane Editor For The Sane People",
	"cli.help.usage":   "Usage: jigedit [FLAGS] [FILENAME]",
	"cli.help.stack":   "[FLAGS] (except -h,-v) can be stacked",
	"cli.help.open":    "  -o, --open      Open file picker to select files",
	"cli.help.config":  "  -c, --config    Open config.json",
	"cli.help.new":     "  -n, --new       Open a new empty tab",
	"cli.help.ro":      "  -R, --readonly  Open subsequent files in READ-ONLY mode",
	"cli.help.edit":    "  -e, --edit      Open subsequent files in EDIT mode (default)",
	"cli.help.version": "  -v, --version   Print version",
	"cli.help.help":    "  -h, --help      Print help",
	"cli.help.example": "\nExample: jigedit -c -o -R file1.txt -e file2.txt -o -R folder/file3.txt",
}

var langKO = map[string]string{
	"lang.name.en": "English",
	"lang.name.ko": "한국어",

	// --- 액션 ---
	"action.open":             "파일 열기",
	"action.save":             "저장",
	"action.save_as":          "다른 이름으로 저장",
	"action.new_tab":          "새 탭 열기",
	"action.close_tab":        "현재 탭 닫기",
	"action.next_tab":         "다음 탭",
	"action.prev_tab":         "이전 탭",
	"action.cycle_tab":        "탭 순환",
	"action.undo":             "실행 취소",
	"action.redo":             "다시 실행",
	"action.select_all":       "모두 선택",
	"action.copy":             "복사",
	"action.cut":              "잘라내기",
	"action.paste":            "붙여넣기",
	"action.find":             "찾기",
	"action.replace":          "바꾸기",
	"action.goto_line":        "줄 이동",
	"action.move_line_up":     "줄 위로 이동",
	"action.move_line_down":   "줄 아래로 이동",
	"action.add_cursor_above": "위에 커서 추가",
	"action.add_cursor_below": "아래에 커서 추가",
	"action.insert_time":      "시간 삽입",
	"action.palette":          "커맨드 팔레트",
	"action.toggle_config":    "설정 파일 편집",
	"action.quit":             "에디터 종료",

	// --- 메뉴 ---
	"menu.palette.title":   " 커맨드 팔레트 ",
	"menu.keybindings":     "단축키 설정",
	"menu.reset_config":    "설정 초기화",
	"menu.toggle_readonly": "읽기 전용 모드 전환",
	"menu.set_language":    "언어 설정",
	"menu.language.title":  " 언어 선택 ",

	"menu.keybind.title":     " 단축키 설정 ",
	"menu.keybind.reset_all": "모든 단축키 기본값으로 되돌리기",
	"menu.keybind.unbound":   "(없음)",
	"menu.keybind.press":     "새 단축키를 누르세요",
	"menu.keybind.hint":      "Esc:취소  Delete:기본값  Backspace:해제",

	"menu.encode.action.title": " 작업 선택 ",
	"menu.encode.region.title": " 지역 선택 ",
	"menu.encode.set_save":     "저장될 인코딩 변경...",
	"menu.encode.reopen":       "다시 열기...",
	"menu.encode.back":         "< 뒤로 가기",

	// --- 인코딩 지역 라벨 ---
	"region.Unicode":             "유니코드",
	"region.Korean":              "한국어",
	"region.Japanese":            "일본어",
	"region.Chinese Simplified":  "중국어 간체",
	"region.Chinese Traditional": "중국어 번체",
	"region.Western European":    "서유럽",
	"region.Central European":    "중앙유럽",
	"region.Cyrillic":            "키릴",
	"region.Greek":               "그리스어",
	"region.Turkish":             "터키어",
	"region.Hebrew":              "히브리어",
	"region.Arabic":              "아랍어",
	"region.Thai":                "태국어",
	"region.Baltic":              "발트",
	"region.Nordic":              "북유럽",

	// --- 경고 프롬프트 ---
	"prompt.quit":            " [Warning] 저장되지 않은 탭이 있습니다. 무시하고 종료할까요? [Y/n]",
	"prompt.close":           " [Warning] 변경된 내용이 있습니다. 탭을 닫을까요? [Y/n]",
	"prompt.reset_config":    " [Warning] 설정을 기본값으로 초기화하시겠습니까? [Y/n]",
	"prompt.reset_keybinds":  " [Warning] 모든 단축키를 기본값으로 되돌리시겠습니까? [Y/n]",
	"prompt.reopen":          " [Warning] 변경된 내용이 있습니다. 무시하고 다시 열까요? [Y/n]",
	"prompt.close_config":    " [Warning] 설정이 저장되지 않았습니다. 무시하고 닫을까요? [Y/n]",
	"prompt.external_change": " [Warning] '%s' 파일이 외부에서 변경되었습니다. 디스크 내용으로 덮어쓸까요? [Y/n]",
	"prompt.alert":           " [Alert] %s (Enter/Esc)",

	// --- 상태 표시줄 ---
	"status.config":       "CONFIG.JSON",
	"status.sel":          "%d자 선택",
	"status.chars":        "%d자",
	"status.palette":      "커맨드 팔레트",
	"status.encode":       "인코딩:",
	"status.readonly":     " | 🔒 읽기 전용",
	"status.new_buffer":   "새 버퍼",
	"status.position":     " | %s | %d행, %d열 | %s%s ",
	"status.tab.untitled": "제목 없음",

	// --- 검색 / 바꾸기 / 줄 이동 표시줄 ---
	"search.goto.prefix":  " [이동] 행,열: ",
	"search.goto.hint":    "  (Enter: 이동, Esc: 취소)",
	"search.replace.find": " [바꾸기] 찾을 내용: ",
	"search.replace.to":   "  ➔ 바꿀 내용: ",
	"search.replace.hint": "] (Enter:바꾸기, Up:이전, Down:건너뛰기, Ctrl+A:모두)",
	"search.find.prefix":  " [찾기] 검색어: ",
	"search.find.hint":    "] (Enter/Down:다음, Up:이전)",

	// --- 단축키 설정 알림 ---
	"keybind.err.palette_locked": "커맨드 팔레트는 해제할 수 없습니다 (되돌릴 방법이 사라집니다)",
	"keybind.err.reserved":       "%s 는 편집기 예약 키라 사용할 수 없습니다",
	"keybind.err.duplicate":      "%s 는 이미 '%s' 에 할당되어 있습니다",

	// --- GUI(zenity) 대화상자 ---
	"dialog.open.title":     "파일 열기",
	"dialog.save_as.title":  "다른 이름으로 저장",
	"dialog.bigfile.title":  "대용량 파일 경고",
	"dialog.bigfile.body":   "파일 크기가 매우 큽니다 (%.1f MB).\n열면 속도가 느려지거나 멈출 수 있습니다. 계속 진행하시겠습니까?",
	"dialog.bigfile.ok":     "예",
	"dialog.bigfile.cancel": "아니오",

	// --- 오류 ---
	"err.config_path":    "설정 경로를 찾을 수 없습니다",
	"err.config_save":    "설정 저장 실패: %v",
	"err.watcher_init":   "파일 감지기를 초기화할 수 없습니다: %v",
	"err.not_regular":    "일반 파일이 아닙니다",
	"err.encode_convert": "인코딩 변환 실패 (저장 중단됨): %v",
	"err.save_failed":    "저장 실패: %v",
	"err.is_directory":   "'%s'은(는) 폴더입니다.",
	"err.no_such_dir":    "경고: 디렉토리가 존재하지 않습니다 (%s)",

	// --- CLI ---
	"cli.version":      "jigedit v1.4.0 - A Sane Editor For The Sane People",
	"cli.help.usage":   "사용법: jigedit [플래그] [파일명]",
	"cli.help.stack":   "[플래그] 는 (-h, -v 를 빼고) 겹쳐 쓸 수 있습니다",
	"cli.help.open":    "  -o, --open      파일 선택 창을 엽니다",
	"cli.help.config":  "  -c, --config    config.json 을 엽니다",
	"cli.help.new":     "  -n, --new       빈 탭을 새로 엽니다",
	"cli.help.ro":      "  -R, --readonly  이후 파일들을 읽기 전용으로 엽니다",
	"cli.help.edit":    "  -e, --edit      이후 파일들을 편집 모드로 엽니다 (기본값)",
	"cli.help.version": "  -v, --version   버전을 출력합니다",
	"cli.help.help":    "  -h, --help      도움말을 출력합니다",
	"cli.help.example": "\n예시: jigedit -c -o -R file1.txt -e file2.txt -o -R folder/file3.txt",
}
