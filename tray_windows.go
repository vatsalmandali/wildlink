//go:build windows

package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// A tray icon in the Windows notification area: click to open the chat,
// right-click for a small menu, and balloon notifications for new messages.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pLoadImageW       = user32.NewProc("LoadImageW")
	pCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	pAppendMenuW      = user32.NewProc("AppendMenuW")
	pTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	pDestroyMenu      = user32.NewProc("DestroyMenu")
	pGetCursorPos     = user32.NewProc("GetCursorPos")
	pSetForegroundWin = user32.NewProc("SetForegroundWindow")
	pShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	trayIcon          notifyIconData
	trayReady         bool
	trayMsg           = uint32(0x0400 + 1) // WM_APP+1
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type notifyIconData struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

type point struct{ x, y int32 }

type msgStruct struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

func utf16Into(dst []uint16, s string) {
	src, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(src) > len(dst) {
		src = append(src[:len(dst)-1], 0)
	}
	copy(dst, src)
}

// startTray installs the notification-area icon. Any failure is non-fatal:
// WildLink keeps working, just without the tray.
func startTray(a *App) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("tray failed: %v", r)
			}
		}()
		runtime.LockOSThread()

		iconPath := filepath.Join(a.dataDir, "wildlink.ico")
		if b, err := uiFS.ReadFile("wildlink.ico"); err == nil {
			os.WriteFile(iconPath, b, 0o644)
		}
		hIcon, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(iconPath))),
			1 /*IMAGE_ICON*/, 0, 0, 0x00000010|0x00000040 /*LR_LOADFROMFILE|LR_DEFAULTSIZE*/)

		hInst, _, _ := pGetModuleHandleW.Call(0)
		className := syscall.StringToUTF16Ptr("WildLinkTrayWindow")
		var proc uintptr
		proc = syscall.NewCallback(func(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
			switch message {
			case trayMsg:
				switch uint32(lParam) {
				case 0x0202, 0x0203, 0x0405: // left click, double click, balloon click
					go openWindow()
				case 0x0205: // right click
					showTrayMenu(hwnd)
				}
				return 0
			case 0x0111: // WM_COMMAND
				switch wParam & 0xffff {
				case 1:
					go openWindow()
				case 2:
					removeTray()
					os.Exit(0)
				}
				return 0
			case 0x0002: // WM_DESTROY
				removeTray()
				return 0
			}
			ret, _, _ := pDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
			return ret
		})
		wc := wndClassEx{
			cbSize: uint32(unsafe.Sizeof(wndClassEx{})), lpfnWndProc: proc,
			hInstance: hInst, lpszClassName: className, hIcon: hIcon,
		}
		if atom, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			return
		}
		hwnd, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(appName))), 0, 0, 0, 0, 0,
			^uintptr(2) /*HWND_MESSAGE = -3*/, 0, hInst, 0)
		if hwnd == 0 {
			return
		}
		trayIcon = notifyIconData{
			cbSize: uint32(unsafe.Sizeof(notifyIconData{})), hWnd: hwnd, uID: 1,
			uFlags: 0x01 | 0x02 | 0x04, uCallbackMessage: trayMsg, hIcon: hIcon, // ICON|MESSAGE|TIP
		}
		utf16Into(trayIcon.szTip[:], appName)
		if r, _, _ := pShellNotifyIconW.Call(0 /*NIM_ADD*/, uintptr(unsafe.Pointer(&trayIcon))); r == 0 {
			return
		}
		trayReady = true
		hasTray = true

		var m msgStruct
		for {
			r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
}

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	pAppendMenuW.Call(menu, 0, 1, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("Open "+appName))))
	pAppendMenuW.Call(menu, 0x800, 0, 0) // separator
	pAppendMenuW.Call(menu, 0, 2, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("Quit "+appName))))
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWin.Call(hwnd)
	pTrackPopupMenu.Call(menu, 0x0080 /*TPM_RIGHTALIGN*/, uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
}

func removeTray() {
	if trayReady {
		pShellNotifyIconW.Call(2 /*NIM_DELETE*/, uintptr(unsafe.Pointer(&trayIcon)))
		trayReady = false
	}
}

// notifyOS shows a notification in the Windows notification area.
func notifyOS(title, body string) {
	if title == "" || !trayReady {
		return
	}
	n := trayIcon
	n.uFlags = 0x10 // NIF_INFO
	utf16Into(n.szInfoTitle[:], title)
	utf16Into(n.szInfo[:], body)
	n.dwInfoFlags = 0x04 // NIIF_USER: use our own icon
	pShellNotifyIconW.Call(1 /*NIM_MODIFY*/, uintptr(unsafe.Pointer(&n)))
}
