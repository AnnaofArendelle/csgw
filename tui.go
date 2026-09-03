package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// 一个够用的上下键菜单：清屏、分隔线、★/☆、反显高亮，↑↓ 选择、Enter 确认、
// Esc 返回、Ctrl-C 退出。没有第三方 TUI 框架，只用 x/term 切换 raw 模式。

const (
	cReset   = "\x1b[0m"
	cBright  = "\x1b[1m"
	cDim     = "\x1b[2m"
	cReverse = "\x1b[7m"
	cAccent  = "\x1b[38;2;217;119;87m" // 赤陶色
	cGreen   = "\x1b[32m"
	cRed     = "\x1b[31m"
)

var errAborted = errors.New("已取消")

// errBack 表示用户按了 Esc。
var errBack = errors.New("返回")

type item struct {
	Label  string
	Detail string
}

func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 20 {
		return 44
	}
	if w > 60 {
		return 60
	}
	return w
}

func rule() string { return cAccent + strings.Repeat("=", termWidth()) + cReset }

// header 画标题区（菜单和普通提示都用它，保证观感一致）。
// 一律用 \r\n：菜单是在 raw 模式下画的，只发 \n 会画成楼梯。
func header(title, subtitle string) {
	fmt.Print("\x1b[2J\x1b[H\r\n")
	fmt.Print(rule() + "\r\n")
	fmt.Printf("  %s%s%s%s\r\n", cBright, cAccent, title, cReset)
	if subtitle != "" {
		fmt.Printf("  %s%s%s\r\n", cDim, strings.ReplaceAll(subtitle, "\n", "\r\n"), cReset)
	}
	fmt.Print(rule() + "\r\n\r\n")
}

// menu 返回选中的下标；Esc 返回 errBack，Ctrl-C 返回 errAborted。
func menu(title, subtitle string, items []item, def int) (int, error) {
	if !isTTY() {
		return 0, errors.New("需要交互式终端")
	}
	if def < 0 || def >= len(items) {
		def = 0
	}
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, err
	}
	defer term.Restore(fd, old)

	sel := def
	draw := func() {
		header(title, subtitle)
		for i, it := range items {
			icon, line := "☆", fmt.Sprintf("  %s %s", "☆", it.Label)
			if i == sel {
				icon = "★"
				line = fmt.Sprintf(" %s%s%s %s%s", cReverse, cBright, icon, it.Label, cReset)
			}
			fmt.Print(line + "\r\n")
			if it.Detail != "" {
				fmt.Printf("      %s%s%s\r\n", cDim, it.Detail, cReset)
			}
		}
		fmt.Printf("\r\n  %s↑↓ 选择   Enter 确认   Esc 返回   Ctrl-C 退出%s\r\n", cDim, cReset)
	}
	draw()

	buf := make([]byte, 32)
	move := func(d int) { sel = (sel + d + len(items)) % len(items) }
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return 0, errAborted
		}
		// 一次 Read 可能带来多个按键（连按方向键就是这样），全部处理掉再重画。
		for i := 0; i < n; {
			b := buf[i]
			switch {
			case b == 3: // Ctrl-C
				return 0, errAborted
			case b == 13 || b == 10: // Enter
				return sel, nil
			case b == 27 && i+2 < n && buf[i+1] == '[':
				switch buf[i+2] {
				case 'A':
					move(-1)
				case 'B':
					move(1)
				}
				i += 3
				continue
			case b == 27: // 单独的 Esc
				return 0, errBack
			case b == 'k':
				move(-1)
			case b == 'j':
				move(1)
			case b == 'q':
				return 0, errBack
			case b >= '1' && b <= '9':
				if idx := int(b - '1'); idx < len(items) {
					return idx, nil
				}
			}
			i++
		}
		draw()
	}
}

// password 读一行不回显的输入。
func password(prompt string) (string, error) {
	fmt.Printf("  %s：", prompt)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// line 读一行普通输入，回车取默认值。
func line(prompt, def string) (string, error) {
	if def != "" {
		fmt.Printf("  %s [%s]：", prompt, def)
	} else {
		fmt.Printf("  %s：", prompt)
	}
	s, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && s == "" {
		return "", errAborted
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

func confirm(prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("  %s [%s]：", prompt, hint)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

func okLine(format string, a ...any) {
	fmt.Printf("  %s✓%s %s\n", cGreen, cReset, fmt.Sprintf(format, a...))
}

func failLine(format string, a ...any) {
	fmt.Printf("  %s✗%s %s\n", cRed, cReset, fmt.Sprintf(format, a...))
}

func pause() {
	fmt.Printf("\n  %s回车继续…%s", cDim, cReset)
	bufio.NewReader(os.Stdin).ReadString('\n')
}
