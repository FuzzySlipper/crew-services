//go:build windows

package winagent

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                      = windows.NewLazySystemDLL("user32.dll")
	gdi32                       = windows.NewLazySystemDLL("gdi32.dll")
	procEnumWindows             = user32.NewProc("EnumWindows")
	procGetWindowThreadProcess  = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible         = user32.NewProc("IsWindowVisible")
	procIsIconic                = user32.NewProc("IsIconic")
	procGetWindowTextLength     = user32.NewProc("GetWindowTextLengthW")
	procGetWindowRect           = user32.NewProc("GetWindowRect")
	procGetDC                   = user32.NewProc("GetDC")
	procReleaseDC               = user32.NewProc("ReleaseDC")
	procPrintWindow             = user32.NewProc("PrintWindow")
	procGetForegroundWindow     = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow     = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop        = user32.NewProc("BringWindowToTop")
	procShowWindow              = user32.NewProc("ShowWindow")
	procAttachThreadInput       = user32.NewProc("AttachThreadInput")
	procSendInput               = user32.NewProc("SendInput")
	procMapVirtualKey           = user32.NewProc("MapVirtualKeyW")
	procGetSystemMetrics        = user32.NewProc("GetSystemMetrics")
	procGetClientRect           = user32.NewProc("GetClientRect")
	procClientToScreen          = user32.NewProc("ClientToScreen")
	procSetCursorPos            = user32.NewProc("SetCursorPos")
	procGetCursorPos            = user32.NewProc("GetCursorPos")
	procSetProcessDpiAwareness  = user32.NewProc("SetProcessDpiAwarenessContext")
	procCreateCompatibleDC      = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap  = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject            = gdi32.NewProc("SelectObject")
	procDeleteObject            = gdi32.NewProc("DeleteObject")
	procDeleteDC                = gdi32.NewProc("DeleteDC")
	procGetDIBits               = gdi32.NewProc("GetDIBits")
	procBitBlt                  = gdi32.NewProc("BitBlt")
	procGetCurrentThreadID      = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentThreadId")
	procWTSGetActiveConsoleSess = windows.NewLazySystemDLL("kernel32.dll").NewProc("WTSGetActiveConsoleSessionId")
)

const (
	createNoWindow      = 0x08000000
	printFullContent    = 2 // PW_RENDERFULLCONTENT: DirectX/DWM-composed content
	swRestore           = 9
	inputMouse          = 0
	inputKeyboard       = 1
	keyExtended         = 0x0001
	keyUp               = 0x0002
	keyScancode         = 0x0008
	mouseMove           = 0x0001
	mouseLeftDown       = 0x0002
	mouseLeftUp         = 0x0004
	mouseRightDown      = 0x0008
	mouseRightUp        = 0x0010
	mouseMiddleDown     = 0x0020
	mouseMiddleUp       = 0x0040
	stillActive         = 259
	dpiPerMonitorAware2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
)

type rect struct{ Left, Top, Right, Bottom int32 }

// INPUT, as SendInput reads it on amd64: a 4-byte type, padding, and a
// 32-byte union.
type keyInput struct {
	Type  uint32
	_     uint32
	Vk    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
	_     [8]byte
}

type mouseInput struct {
	Type  uint32
	_     uint32
	Dx    int32
	Dy    int32
	Data  uint32
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
}

// Windows is the real desktop of the session the agent runs in.
type Windows struct {
	mu   sync.Mutex
	held map[string]func() error
}

func NewDesktop() *Windows {
	// Physical pixels for window rectangles and captures.
	procSetProcessDpiAwareness.Call(dpiPerMonitorAware2)
	return &Windows{held: map[string]func() error{}}
}

func (d *Windows) Start(command string, args []string, dir string, env []string, log string) (int, error) {
	file, err := os.Create(log)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(command, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, file, file
	// No console window: only the product's own window appears.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow | syscall.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		file.Close()
		return 0, err
	}
	go func() { _ = cmd.Wait(); file.Close() }()
	return cmd.Process.Pid, nil
}

// Stop ends the process tree: rusty dev, its build tools and the host.
func (d *Windows) Stop(pid int) error {
	output, err := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill %d: %w: %s", pid, err, output)
	}
	return nil
}

func (d *Windows) Image(pid int) string {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size) != nil {
		return ""
	}
	path := windows.UTF16ToString(buffer[:size])
	return path[strings.LastIndexAny(path, `\/`)+1:]
}

func (d *Windows) Alive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	return windows.GetExitCodeProcess(handle, &code) == nil && code == stillActive
}

// tree is pid and every process descended from it.
func tree(pid int) (map[uint32]bool, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	parents := map[uint32]uint32{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parents[entry.ProcessID] = entry.ParentProcessID
	}
	members := map[uint32]bool{uint32(pid): true}
	for changed := true; changed; {
		changed = false
		for child, parent := range parents {
			if members[parent] && !members[child] {
				members[child], changed = true, true
			}
		}
	}
	return members, nil
}

func (d *Windows) Window(pid int) (uintptr, error) {
	members, err := tree(pid)
	if err != nil {
		return 0, err
	}
	var best uintptr
	var bestArea int64
	callback := windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var owner uint32
		procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if !members[owner] {
			return 1
		}
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}
		if length, _, _ := procGetWindowTextLength.Call(hwnd); length == 0 {
			return 1
		}
		var r rect
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
		if area := int64(r.Right-r.Left) * int64(r.Bottom-r.Top); area > bestArea {
			best, bestArea = hwnd, area
		}
		return 1
	})
	procEnumWindows.Call(callback, 0)
	if best == 0 {
		return 0, fmt.Errorf("no visible window in process tree %d yet", pid)
	}
	return best, nil
}

func (d *Windows) CapturePNG(window uintptr) ([]byte, error) {
	if iconic, _, _ := procIsIconic.Call(window); iconic != 0 {
		return nil, errors.New("the window is minimized; Windows draws nothing to capture")
	}
	var r rect
	procGetWindowRect.Call(window, uintptr(unsafe.Pointer(&r)))
	width, height := int(r.Right-r.Left), int(r.Bottom-r.Top)
	if width <= 0 || height <= 0 {
		return nil, errors.New("the window has no area")
	}
	screen, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, screen)
	memory, _, _ := procCreateCompatibleDC.Call(screen)
	defer procDeleteDC.Call(memory)
	bitmap, _, _ := procCreateCompatibleBitmap.Call(screen, uintptr(width), uintptr(height))
	defer procDeleteObject.Call(bitmap)
	previous, _, _ := procSelectObject.Call(memory, bitmap)
	ok, _, err := procPrintWindow.Call(window, memory, printFullContent)
	procSelectObject.Call(memory, previous)
	if ok == 0 {
		return nil, fmt.Errorf("PrintWindow: %v", err)
	}
	return bitmapPNG(memory, bitmap, width, height)
}

// CaptureDesktopPNG copies the primary screen as the desktop shows it.
func (d *Windows) CaptureDesktopPNG() ([]byte, error) {
	width, height := screenSize()
	screen, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, screen)
	memory, _, _ := procCreateCompatibleDC.Call(screen)
	defer procDeleteDC.Call(memory)
	bitmap, _, _ := procCreateCompatibleBitmap.Call(screen, uintptr(width), uintptr(height))
	defer procDeleteObject.Call(bitmap)
	previous, _, _ := procSelectObject.Call(memory, bitmap)
	ok, _, err := procBitBlt.Call(memory, 0, 0, uintptr(width), uintptr(height), screen, 0, 0, srcCopy|captureBlt)
	procSelectObject.Call(memory, previous)
	if ok == 0 {
		return nil, fmt.Errorf("BitBlt: %v", err)
	}
	return bitmapPNG(memory, bitmap, width, height)
}

const (
	srcCopy    = 0x00CC0020
	captureBlt = 0x40000000
)

func screenSize() (int, int) {
	width, _, _ := procGetSystemMetrics.Call(0)  // SM_CXSCREEN
	height, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
	return int(width), int(height)
}

// bitmapPNG encodes a captured bitmap as an opaque PNG.
func bitmapPNG(memory, bitmap uintptr, width, height int) ([]byte, error) {
	// BITMAPINFOHEADER for top-down 32-bit BGRA rows.
	header := struct {
		Size                         uint32
		Width, Height                int32
		Planes, BitCount             uint16
		Compression, SizeImage       uint32
		XPels, YPels, Used, Required int32
	}{Size: 40, Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32}
	pixels := make([]byte, width*height*4)
	if lines, _, err := procGetDIBits.Call(memory, bitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&pixels[0])), uintptr(unsafe.Pointer(&header)), 0); lines == 0 {
		return nil, fmt.Errorf("GetDIBits: %v", err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < len(pixels); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = pixels[i+2], pixels[i+1], pixels[i], 255
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Foreground brings window to the front. The agent runs in the interactive
// session; attaching to the current foreground thread's input satisfies the
// foreground-permission rules without stealing focus from a person's click.
func (d *Windows) Foreground(window uintptr) (bool, error) {
	if current, _, _ := procGetForegroundWindow.Call(); current == window {
		centreCursor(window)
		return true, nil
	}
	if iconic, _, _ := procIsIconic.Call(window); iconic != 0 {
		procShowWindow.Call(window, swRestore)
	}
	foreground, _, _ := procGetForegroundWindow.Call()
	foregroundThread, _, _ := procGetWindowThreadProcess.Call(foreground, 0)
	self, _, _ := procGetCurrentThreadID.Call()
	if foregroundThread != 0 && foregroundThread != self {
		procAttachThreadInput.Call(self, foregroundThread, 1)
		defer procAttachThreadInput.Call(self, foregroundThread, 0)
	}
	procBringWindowToTop.Call(window)
	procSetForegroundWindow.Call(window)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if current, _, _ := procGetForegroundWindow.Call(); current == window {
			centreCursor(window)
			return true, nil
		}
	}
	return false, errors.New("Windows kept another window in front")
}

// centreCursor puts the pointer over the window's client area when it is
// elsewhere, so clicks land on the product (and can take its pointer lock).
// A pointer already inside, locked or not, is left where it is.
func centreCursor(window uintptr) {
	var client rect
	procGetClientRect.Call(window, uintptr(unsafe.Pointer(&client)))
	origin := struct{ X, Y int32 }{}
	procClientToScreen.Call(window, uintptr(unsafe.Pointer(&origin)))
	var cursor struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	if cursor.X >= origin.X && cursor.X < origin.X+client.Right && cursor.Y >= origin.Y && cursor.Y < origin.Y+client.Bottom {
		return
	}
	procSetCursorPos.Call(uintptr(origin.X+client.Right/2), uintptr(origin.Y+client.Bottom/2))
}

func sendInputs(inputs ...any) error {
	for _, item := range inputs {
		var pointer unsafe.Pointer
		switch v := item.(type) {
		case *keyInput:
			pointer = unsafe.Pointer(v)
		case *mouseInput:
			pointer = unsafe.Pointer(v)
		}
		if sent, _, err := procSendInput.Call(1, uintptr(pointer), 40); sent != 1 {
			return fmt.Errorf("SendInput: %v", err)
		}
	}
	return nil
}

// Keys go as scan codes, so raw-input readers (winit) see the physical key.
func key(vk int, up bool) *keyInput {
	scan, _, _ := procMapVirtualKey.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
	flags := uint32(keyScancode)
	switch vk {
	case 0x25, 0x26, 0x27, 0x28, 0xA3, 0xA5, 0x2D, 0x2E, 0x24, 0x23, 0x21, 0x22:
		flags |= keyExtended
	}
	if up {
		flags |= keyUp
	}
	return &keyInput{Type: inputKeyboard, Scan: uint16(scan), Flags: flags}
}

var buttons = map[string][2]uint32{"left": {mouseLeftDown, mouseLeftUp}, "right": {mouseRightDown, mouseRightUp}, "middle": {mouseMiddleDown, mouseMiddleUp}}

func (d *Windows) hold(id string, release func() error) {
	d.mu.Lock()
	d.held[id] = release
	d.mu.Unlock()
}

func (d *Windows) lift(id string) error {
	d.mu.Lock()
	release := d.held[id]
	delete(d.held, id)
	d.mu.Unlock()
	if release == nil {
		return nil
	}
	return release()
}

// Send delivers a validated batch: hold (virtual keys), move (relative
// motion spread over ms), click, wait. Absolute points and gamepads are not
// OS-tier input here.
func (d *Windows) Send(steps []map[string]any, window uintptr) (map[string]any, error) {
	for _, step := range steps {
		if kind := step["kind"]; kind == "gamepad" {
			return map[string]any{"completed_steps": 0}, fmt.Errorf("capability_unavailable: %v steps are not OS-tier input", kind)
		}
	}
	completed := 0
	for _, step := range steps {
		ms := number(step["ms"])
		switch step["kind"] {
		case "wait":
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "point":
			// x, y are a position in a width x height image of the window's
			// capture, or of the desktop's.
			var area rect
			if window != 0 {
				procGetWindowRect.Call(window, uintptr(unsafe.Pointer(&area)))
			} else {
				w, h := screenSize()
				area = rect{Right: int32(w), Bottom: int32(h)}
			}
			x := int(area.Left) + number(step["x"])*int(area.Right-area.Left)/number(step["width"])
			y := int(area.Top) + number(step["y"])*int(area.Bottom-area.Top)/number(step["height"])
			if ok, _, err := procSetCursorPos.Call(uintptr(x), uintptr(y)); ok == 0 {
				return map[string]any{"completed_steps": completed}, fmt.Errorf("SetCursorPos: %v", err)
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "hold":
			keys := step["keys"].([]any)
			for _, k := range keys {
				vk := number(k)
				if err := sendInputs(key(vk, false)); err != nil {
					return map[string]any{"completed_steps": completed}, err
				}
				d.hold(fmt.Sprint("vk:", vk), func() error { return sendInputs(key(vk, true)) })
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			for i := len(keys) - 1; i >= 0; i-- {
				if err := d.lift(fmt.Sprint("vk:", number(keys[i]))); err != nil {
					return map[string]any{"completed_steps": completed}, err
				}
			}
		case "move":
			dx, dy := number(step["dx"]), number(step["dy"])
			parts := max(1, ms/16)
			for i := 0; i < parts; i++ {
				x, y := dx*(i+1)/parts-dx*i/parts, dy*(i+1)/parts-dy*i/parts
				if err := sendInputs(&mouseInput{Type: inputMouse, Dx: int32(x), Dy: int32(y), Flags: mouseMove}); err != nil {
					return map[string]any{"completed_steps": completed}, err
				}
				if parts > 1 {
					time.Sleep(time.Duration(ms/parts) * time.Millisecond)
				}
			}
		case "click":
			// Batches number buttons 1 left, 2 middle, 3 right (default 1).
			name := map[int]string{0: "left", 1: "left", 2: "middle", 3: "right"}[number(step["button"])]
			flags, ok := buttons[name]
			if !ok {
				return map[string]any{"completed_steps": completed}, fmt.Errorf("click button %v is not 1..3", step["button"])
			}
			if err := sendInputs(&mouseInput{Type: inputMouse, Flags: flags[0]}); err != nil {
				return map[string]any{"completed_steps": completed}, err
			}
			d.hold("button:"+name, func() error { return sendInputs(&mouseInput{Type: inputMouse, Flags: flags[1]}) })
			time.Sleep(time.Duration(ms) * time.Millisecond)
			if err := d.lift("button:" + name); err != nil {
				return map[string]any{"completed_steps": completed}, err
			}
		case "down", "up", "wheel":
			return map[string]any{"completed_steps": completed}, fmt.Errorf("capability_unavailable: %v is not delivered by the Windows desktop yet", step["kind"])
		}
		completed++
	}
	return map[string]any{"completed_steps": completed}, nil
}

func (d *Windows) Release() error {
	d.mu.Lock()
	held := d.held
	d.held = map[string]func() error{}
	d.mu.Unlock()
	var errs []error
	for _, release := range held {
		errs = append(errs, release())
	}
	return errors.Join(errs...)
}

func (d *Windows) Facts() map[string]any {
	width, _, _ := procGetSystemMetrics.Call(0)
	height, _, _ := procGetSystemMetrics.Call(1)
	var session uint32
	_ = windows.ProcessIdToSessionId(uint32(os.Getpid()), &session)
	console, _, _ := procWTSGetActiveConsoleSess.Call()
	foreground, _, _ := procGetForegroundWindow.Call()
	return map[string]any{"screen": fmt.Sprintf("%dx%d", width, height), "session": session, "console_session": uint32(console) == session,
		"foreground_window": foreground != 0}
}
