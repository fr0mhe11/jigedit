package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
)

const (
	// maxOSC52Bytes 는 터미널 에뮬레이터(xterm/tmux/VTE 등)의 OSC 시퀀스 버퍼 한계를
	// 초과하지 않도록 OSC 52 전송을 허용하는 최대 바이트 크기(100KB)다.
	maxOSC52Bytes = 100 * 1024

	// extClipTimeout 은 외부 클립보드 유틸리티가 응답하지 않을 때 에디터가 멈추지 않도록
	// 강제 종료하는 최대 대기 시간이다.
	extClipTimeout = 500 * time.Millisecond
)

var errExternalClipTimeout = errors.New("external clipboard timeout")

// ClipboardManager 는 외부 패키지(wl-clipboard / xclip)가 없어도 지연 없이 동작하는
// 경량 클립보드 매니저다:
//  1. 복사/잘라내기: 내부 버퍼 저장 + (설치 시) 외부 CLI 기록 + 터미널 OSC 52 쓰기 (0ms)
//  2. 붙여넣기(Ctrl+V): (설치 시) 외부 CLI 읽기 -> 없으면 즉시 내부 버퍼 반환 (0ms, 대기 없음)
//  3. 외부 앱 -> 에디터 붙여넣기(외부 CLI 미설치 시): 터미널 Bracketed Paste (Ctrl+Shift+V)
type ClipboardManager struct {
	mu              sync.Mutex
	internal        string
	disableExternal bool
	extDetected     bool
	extSpec         extClipCmd
	extAvailable    bool
}

var globalClipboard = &ClipboardManager{}

func resolveScreen(s tcell.Screen) tcell.Screen {
	if s != nil {
		return s
	}
	if globalScreenHandle != nil {
		return *globalScreenHandle
	}
	return nil
}

func (c *ClipboardManager) cachedExternalSpec() (extClipCmd, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disableExternal {
		return extClipCmd{}, false
	}
	if !c.extDetected {
		c.extSpec, c.extAvailable = detectExternalClipboard()
		c.extDetected = true
	}
	return c.extSpec, c.extAvailable
}

func (c *ClipboardManager) resetExternalCache() {
	c.mu.Lock()
	c.extDetected = false
	c.extSpec = extClipCmd{}
	c.extAvailable = false
	c.mu.Unlock()
}

// Write 는 텍스트를 내부 버퍼에 저장하고, 설치된 시스템 도구가 있으면 기록한 뒤,
// 터미널에 단방향 OSC 52 시퀀스를 전송한다.
func (c *ClipboardManager) Write(s tcell.Screen, text string) {
	if text == "" {
		return
	}

	c.mu.Lock()
	c.internal = text
	c.mu.Unlock()

	if spec, ok := c.cachedExternalSpec(); ok {
		if err := writeExternalClipboardWithSpec(spec, text); errors.Is(err, errExternalClipTimeout) {
			c.disableExternalBackend()
		}
	}

	if scr := resolveScreen(s); scr != nil && len(text) <= maxOSC52Bytes {
		scr.SetClipboard([]byte(text))
	}
}

func (c *ClipboardManager) disableExternalBackend() {
	c.mu.Lock()
	c.extDetected = true
	c.extAvailable = false
	c.mu.Unlock()
}

// Internal 은 에디터 내부에 마지막으로 복사/잘라내기된 텍스트를 반환한다.
func (c *ClipboardManager) Internal() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.internal
}

// ReadExternal 은 현재 환경에 적합한 외부 클립보드 유틸리티가 설치되어 있을 때만
// 읽어 오고, 없거나 실패하면 ("", false)를 반환한다.
func (c *ClipboardManager) ReadExternal() (string, bool) {
	spec, ok := c.cachedExternalSpec()
	if !ok {
		return "", false
	}
	text, err := readExternalClipboardWithSpec(spec)
	if err != nil {
		if errors.Is(err, errExternalClipTimeout) {
			c.disableExternalBackend()
		}
		return "", false
	}
	return text, true
}

// ReadSync 는 동기적으로 즉시 클립보드 텍스트(외부 도구 -> 내부 버퍼 순)를 반환한다.
// 외부 도구가 없으면 대기 없이 0ms 로 내부 버퍼를 돌려준다.
func (c *ClipboardManager) ReadSync() string {
	if text, ok := c.ReadExternal(); ok && text != "" {
		return text
	}
	return c.Internal()
}

type extClipCmd struct {
	copyArgs  []string
	pasteArgs []string
	trimCRLF  bool
}

// detectExternalClipboard 는 현재 OS 및 디스플레이 세션에 맞는 외부 클립보드 명령을 찾는다.
// Wayland 에서는 wl-clipboard 를 우선하되, 미설치 상태이고 XWayland(DISPLAY)가 활성화된
// 경우에는 설치된 xclip/xsel 도 폴백으로 허용한다.
func detectExternalClipboard() (extClipCmd, bool) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("pbcopy"); err == nil {
			if _, err := exec.LookPath("pbpaste"); err == nil {
				return extClipCmd{
					copyArgs:  []string{"pbcopy"},
					pasteArgs: []string{"pbpaste"},
				}, true
			}
		}
		return extClipCmd{}, false

	case "windows":
		if _, err := exec.LookPath("clip.exe"); err == nil {
			if _, err := exec.LookPath("powershell.exe"); err == nil {
				return extClipCmd{
					copyArgs:  []string{"clip.exe"},
					pasteArgs: []string{"powershell.exe", "-NoProfile", "-Command", "Get-Clipboard"},
					trimCRLF:  true,
				}, true
			}
		}
		return extClipCmd{}, false

	default:
		// 1. Wayland 세션: wl-copy / wl-paste 최우선
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			if _, err := exec.LookPath("wl-copy"); err == nil {
				if _, err := exec.LookPath("wl-paste"); err == nil {
					return extClipCmd{
						copyArgs:  []string{"wl-copy"},
						pasteArgs: []string{"wl-paste", "--no-newline"},
					}, true
				}
			}
		}

		// 2. X11 또는 XWayland 세션: DISPLAY 가 설정된 경우 xclip 또는 xsel 사용
		if os.Getenv("DISPLAY") != "" {
			if _, err := exec.LookPath("xclip"); err == nil {
				return extClipCmd{
					copyArgs:  []string{"xclip", "-in", "-selection", "clipboard"},
					pasteArgs: []string{"xclip", "-out", "-selection", "clipboard"},
				}, true
			}
			if _, err := exec.LookPath("xsel"); err == nil {
				return extClipCmd{
					copyArgs:  []string{"xsel", "--input", "--clipboard"},
					pasteArgs: []string{"xsel", "--output", "--clipboard"},
				}, true
			}
		}

		// 3. Termux 환경
		if _, err := exec.LookPath("termux-clipboard-set"); err == nil {
			if _, err := exec.LookPath("termux-clipboard-get"); err == nil {
				return extClipCmd{
					copyArgs:  []string{"termux-clipboard-set"},
					pasteArgs: []string{"termux-clipboard-get"},
				}, true
			}
		}

		// 4. WSL 환경
		if _, err := exec.LookPath("clip.exe"); err == nil {
			if _, err := exec.LookPath("powershell.exe"); err == nil {
				return extClipCmd{
					copyArgs:  []string{"clip.exe"},
					pasteArgs: []string{"powershell.exe", "-NoProfile", "-Command", "Get-Clipboard"},
					trimCRLF:  true,
				}, true
			}
		}

		return extClipCmd{}, false
	}
}

func writeExternalClipboardWithSpec(spec extClipCmd, text string) error {
	if len(spec.copyArgs) == 0 {
		return errors.New("no external clipboard utility")
	}

	cmd := exec.Command(spec.copyArgs[0], spec.copyArgs[1:]...)
	// wl-copy / xclip 은 백그라운드 데몬을 포크해 선택 소유권을 유지하므로
	// stdout/stderr 파이프를 연결하지 않아야 Wait() 가 즉시 반환된다.
	cmd.Stdout = nil
	cmd.Stderr = nil
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return err
	}

	done := make(chan error, 1)
	go func() {
		_, wErr := in.Write([]byte(text))
		cErr := in.Close()
		waitErr := cmd.Wait()
		if wErr != nil {
			done <- wErr
		} else if cErr != nil {
			done <- cErr
		} else {
			done <- waitErr
		}
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(extClipTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return errExternalClipTimeout
	}
}

func readExternalClipboardWithSpec(spec extClipCmd) (string, error) {
	if len(spec.pasteArgs) == 0 {
		return "", errors.New("no external clipboard utility")
	}

	cmd := exec.Command(spec.pasteArgs[0], spec.pasteArgs[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return "", err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
		res := out.String()
		if spec.trimCRLF {
			res = strings.TrimSuffix(res, "\r\n")
		}
		return res, nil
	case <-time.After(extClipTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", errExternalClipTimeout
	}
}

// --- [ 에디터 / 검색바 / 파일 피커 붙여넣기 통합 헬퍼 ] ---

func firstNonEmptyLine(s string) string {
	s = strings.TrimLeft(s, "\r\n")
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = s[:idx]
	}
	return s
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (e *Editor) pasteIntoBuffer(text string) bool {
	b := e.getActive()
	if b == nil || b.isReadOnly || text == "" {
		return false
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	b.stickToWrapEnd = false
	b.cursor.TargetX = -1
	for i := range b.extraCursors {
		b.extraCursors[i].TargetX = -1
	}
	b.BeginTransaction()
	b.DeleteSelection()
	// ponytail(CM6): if the clipboard splits into exactly one line per
	// live caret, give each caret its own line (in document order)
	// instead of pasting the whole blob at every caret.
	cursorCount := 1 + len(b.extraCursors)
	parts := strings.Split(text, "\n")
	if cursorCount > 1 && len(parts) == cursorCount {
		rank := b.cursorRankMap()
		b.runMultiCursorInsert(func(loc Loc, _ bool) string { return parts[rank[loc]] })
	} else {
		b.runMultiCursorInsert(func(Loc, bool) string { return text })
	}
	b.EndTransaction()
	return true
}

func (e *Editor) pasteIntoSearchOrGoto(text string) bool {
	b := e.getActive()
	if b == nil || (!b.searchMode && !b.gotoMode) || text == "" {
		return false
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
	if targetStr == nil {
		return false
	}

	n := len(*targetStr)
	b.inputCX = clampInt(b.inputCX, 0, n)

	if b.isInputSelect && b.inputSelStart != b.inputSelEnd {
		selStart := clampInt(b.inputSelStart, 0, n)
		selEnd := clampInt(b.inputSelEnd, 0, n)
		if selStart > selEnd {
			selStart, selEnd = selEnd, selStart
		}
		if selStart != selEnd {
			*targetStr = append((*targetStr)[:selStart], (*targetStr)[selEnd:]...)
			b.inputCX = selStart
		}
		b.isInputSelect = false
		b.inputSelStart = 0
		b.inputSelEnd = 0
	}

	text = strings.ReplaceAll(text, "\r\n", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", "")
	if text == "" {
		return false
	}

	runes := []rune(text)
	b.inputCX = clampInt(b.inputCX, 0, len(*targetStr))
	*targetStr = append((*targetStr)[:b.inputCX], append(runes, (*targetStr)[b.inputCX:]...)...)
	b.inputCX += len(runes)
	if b.searchMode && (!b.isReplace || b.replaceStep == 1) {
		b.matches = nil
		b.matchIdx = -1
	}
	return true
}

func (fp *FilePicker) pasteText(clip string) bool {
	if fp == nil {
		return false
	}
	clip = firstNonEmptyLine(clip)
	if clip == "" {
		return false
	}
	fp.errorMsg = ""

	switch fp.focus {
	case fpFocusPath:
		fp.pathInput.insertString(clip)
		return true
	case fpFocusNewDir:
		fp.newDirInput.insertString(clip)
		return true
	case fpFocusFilter:
		before := fp.filterInput.text()
		fp.filterInput.insertString(clip)
		if fp.filterInput.text() != before {
			fp.applyFilter("")
		}
		return true
	case fpFocusFilename:
		fp.filenameInput.insertString(clip)
		return true
	case fpFocusList:
		if fp.mode == fpModeSaveAs {
			fp.focus = fpFocusFilename
			fp.filenameInput.insertString(clip)
		} else {
			before := fp.filterInput.text()
			fp.filterInput.insertString(clip)
			if fp.filterInput.text() != before {
				fp.applyFilter("")
			}
		}
		return true
	default:
		return false
	}
}

func (e *Editor) applyPastedText(text string) (bool, bool) {
	if text == "" || e.confirmDialog != nil {
		return false, false
	}
	if e.filePicker != nil {
		return e.filePicker.pasteText(text), false
	}
	if e.promptMode || e.langMenuActive || e.paletteActive || e.ctxMenuActive || e.encodeMenuActive || e.keyMenuActive {
		return false, false
	}
	b := e.getActive()
	if b != nil && (b.searchMode || b.gotoMode) {
		return e.pasteIntoSearchOrGoto(text), false
	}
	if e.pasteIntoBuffer(text) {
		return true, true
	}
	return false, false
}

func (e *Editor) handlePasteEvent(ev *tcell.EventPaste) (bool, bool) {
	if ev.Start() {
		e.inBracketedPaste = true
		e.pasteBuf.Reset()
		e.pasteLastCR = false
		return false, false
	}
	if !e.inBracketedPaste {
		return false, false
	}
	e.inBracketedPaste = false
	text := e.pasteBuf.String()
	e.pasteBuf.Reset()
	e.pasteLastCR = false
	return e.applyPastedText(text)
}

func (e *Editor) handleBracketedPasteKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEnter:
		e.pasteBuf.WriteByte('\n')
		e.pasteLastCR = true
	case tcell.KeyCtrlJ:
		if !e.pasteLastCR {
			e.pasteBuf.WriteByte('\n')
		}
		e.pasteLastCR = false
	case tcell.KeyTab:
		e.pasteBuf.WriteByte('\t')
		e.pasteLastCR = false
	case tcell.KeyRune:
		r := ev.Rune()
		if r == '\r' {
			e.pasteBuf.WriteByte('\n')
			e.pasteLastCR = true
		} else if r == '\n' {
			if !e.pasteLastCR {
				e.pasteBuf.WriteByte('\n')
			}
			e.pasteLastCR = false
		} else if r == '\t' || (r >= 0x20 && r != 0x7f) {
			e.pasteBuf.WriteRune(r)
			e.pasteLastCR = false
		} else {
			e.pasteLastCR = false
		}
	default:
		e.pasteLastCR = false
	}
}
