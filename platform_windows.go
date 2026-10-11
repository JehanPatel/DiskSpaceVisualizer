//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const trashName = "Recycle Bin"
const canPickFolder = true

var extraSkipNames = map[string]bool{}

func systemSkipPaths() []string {
	return []string{}
}

func listDrives() []string {
	var out []string
	for c := 'A'; c <= 'Z'; c++ {
		p := string(c) + `:\`
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

type shFileOpStruct struct {
	Hwnd                 uintptr
	Func                 uint32
	From                 *uint16
	To                   *uint16
	Flags                uint16
	AnyOperationsAborted int32
	NameMappings         uintptr
	ProgressTitle        *uint16
}

var procSHFileOperation = syscall.NewLazyDLL("shell32.dll").NewProc("SHFileOperationW")

func moveToTrash(path string) error {
	from, err := syscall.UTF16FromString(path)
	if err != nil {
		return err
	}
	from = append(from, 0)

	const (
		foDelete          = 0x0003
		fofSilent         = 0x0004
		fofNoConfirmation = 0x0010
		fofAllowUndo      = 0x0040
		fofNoErrorUI      = 0x0400
	)
	op := shFileOpStruct{
		Func:  foDelete,
		From:  &from[0],
		Flags: fofSilent | fofNoConfirmation | fofAllowUndo | fofNoErrorUI,
	}
	r, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(from)
	if r != 0 {
		return fmt.Errorf("Windows could not move this file to the Recycle Bin (error 0x%X).", r)
	}
	if op.AnyOperationsAborted != 0 {
		return errors.New("Windows cancelled the move to the Recycle Bin.")
	}
	return nil
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	_ = cmd.Start()
}

func pickFolder() (string, error) {
	script := `[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false; ` +
		`Add-Type -AssemblyName System.Windows.Forms; ` +
		`$f = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$f.Description = 'Choose a folder to visualize'; ` +
		`$f.ShowNewFolderButton = $false; ` +
		`$owner = New-Object System.Windows.Forms.Form; $owner.TopMost = $true; ` +
		`if ($f.ShowDialog($owner) -eq 'OK') { [Console]::Out.Write($f.SelectedPath) }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("The folder dialog could not open. Type the folder path instead.")
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff")), nil
}

func revealPath(path string) error {
	if path == "" {
		return errors.New("No file path was provided.")
	}
	if _, err := os.Stat(path); err != nil {
		return errors.New("That file is not available anymore.")
	}
	cmd := exec.Command("explorer.exe", "/select,"+path)
	hideWindow(cmd)
	return cmd.Start()
}
