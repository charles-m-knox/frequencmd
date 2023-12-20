package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrg/xdg"
	"github.com/gdamore/tcell/v2"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/rivo/tview"
	"gopkg.in/yaml.v3"
)

const (
	maxLogLines = 10000
	BOX_LIST    = "list"
	BOX_STDOUT  = "stdout"
	BOX_STDERR  = "stderr"
)

var (
	lastCmd                *Command
	commands               *[]Command
	list                   *tview.List
	layout                 *tview.Flex
	info                   *tview.TextView
	errors                 *tview.TextView
	exitCode               *tview.TextView
	bottomLeftText         *tview.TextView
	bottomLeftSearch       *tview.InputField
	bottomLeftBox          *tview.Box
	globalProcessesRunning int
	stdoutLines            []string
	stderrLines            []string
	app                    *tview.Application
	runIndex               sync.Map
	isSearching            bool
	searchTerm             string
	keybindings            []Keybinding
	config                 Config
	currentlyFocusedBox    string
	filteredResults        []string
)

type Config struct {
	Commands                    []ConfigCommand `yaml:"commands"`
	Keybindings                 []Keybinding    `yaml:"keybindings"`
	IdleRefreshRateMs           int             `yaml:"idleRefreshRateMs"`
	ProcessRunningRefreshRateMs int             `yaml:"processRunningRefreshRateMs"`
}

type Keybinding struct {
	Action     string
	Keybinding string
}

func loadConfig() (c Config, err error) {
	// TODO: later on, try current dir, then xdg_config_dir, then xdg_user_dir
	xdgConfig := path.Join(xdg.ConfigHome, "frequencmd", "config.yml")
	xdgHome := path.Join(xdg.Home, "frequencmd", "config.yml")
	curConf := "config.yml"

	b, err := os.ReadFile(curConf)
	if err == nil {
		err = yaml.Unmarshal(b, &c)
		if err != nil {
			return c, fmt.Errorf(
				"failed to read config from %v: %v",
				curConf,
				err.Error(),
			)
		}
		return c, nil
	}

	b, err = os.ReadFile(xdgConfig)
	if err == nil {
		err = yaml.Unmarshal(b, &c)
		if err != nil {
			return c, fmt.Errorf(
				"failed to read config from %v: %v",
				xdgConfig,
				err.Error(),
			)
		}
		return c, nil
	}

	b, err = os.ReadFile(xdgHome)
	if err == nil {
		err = yaml.Unmarshal(b, &c)
		if err != nil {
			return c, fmt.Errorf(
				"failed to read config from %v: %v",
				xdgHome,
				err.Error(),
			)
		}
		return c, nil
	}

	return c, fmt.Errorf(
		"failed to read config from %v, %v, and %v: %v",
		curConf,
		xdgConfig,
		xdgHome,
		err.Error(),
	)
}

func parseConfigCommands(conf Config) {
	commands = &[]Command{}
	for i := range conf.Commands {
		c := conf.Commands[i]

		n := Command{
			Command: c.Command,
			Env:     c.Env,
			Label:   c.Label,
		}

		if c.Shell != "" {
			n.Args = []string{"-c", c.Shell}
		} else {
			n.Args = strings.Split(c.Args, " ")
		}
		*commands = append(*commands, n)
	}
}

func logOutput(output io.ReadCloser, lines *[]string, prefix string, view *tview.TextView, color tcell.Color) {
	*lines = []string{}
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		line := scanner.Text()
		*lines = append(*lines, line)
		var sb strings.Builder
		linesLen := len(*lines)
		for i := linesLen - 1; i >= linesLen-maxLogLines && i >= 0; i-- {
			fmt.Fprintf(&sb, "%v%v:[%v] %v\n", prefix, getNowStr(), color, (*lines)[i])
		}
		view.SetText(sb.String())
		app.QueueUpdateDraw(func() {})
		// log.Print(line)
	}
}

func getNowStr() string {
	return time.Now().Format("15:04:05")
}

func setLastCommandText(cmd *Command) {
	lastCmd = cmd
	exitCode.SetTitle(fmt.Sprintf("exit code for: %v", cmd.Label))
}

func setBottomLeftText(t string) {
	bottomLeftText.SetText(fmt.Sprintf("[white][ctrl+c][gray] to quit | %v", t))
}

func pidRunningDrawLoop() {
	for {
		// setStatusText(fmt.Sprintf("%v loops", loops))
		sleepTime := time.Duration(config.IdleRefreshRateMs) * time.Millisecond

		processesRunning := 0
		keysToDelete := []int64{}
		shouldRedrawApp := false
		runIndex.Range(func(key, value any) bool {
			if value == true {
				if app != nil {
					shouldRedrawApp = true
				}

				processesRunning += 1
				errors.ScrollToEnd()
				info.ScrollToEnd()
			} else {
				keysToDelete = append(keysToDelete, key.(int64))
			}

			return true
		})

		// last process finished running; do a one-time update on the status bar
		if processesRunning == 0 && globalProcessesRunning > 0 {
			globalProcessesRunning = 0
			setBottomLeftText(fmt.Sprintf("%v running", globalProcessesRunning))
			app.QueueUpdateDraw(func() {})
		}

		// sync up with the global counter
		globalProcessesRunning = processesRunning
		if globalProcessesRunning > 0 {
			setBottomLeftText(fmt.Sprintf("%v running", globalProcessesRunning))
			app.QueueUpdateDraw(func() {})
		}

		if shouldRedrawApp {
			// draw a little faster if we know something is running
			sleepTime = time.Duration(config.ProcessRunningRefreshRateMs) * time.Millisecond
			setBottomLeftText(fmt.Sprintf("%v running", globalProcessesRunning))
			app.QueueUpdateDraw(func() {})
		}

		for key := range keysToDelete {
			keyToDelete := key
			runIndex.Delete(keyToDelete)
		}

		time.Sleep(sleepTime)
	}
}

func runCommand(command *Command /* command string, args []string, env []string */) {
	jobId := time.Now().UnixNano()
	runIndex.Store(jobId, true)

	setLastCommandText(command)
	exitCode.SetText(fmt.Sprintf("[gray]%v [aqua] running command:[white] %v", getNowStr(), command.Label))

	info.Clear()
	errors.Clear()

	cmd := exec.Command(command.Command, command.Args...)
	cmd.Env = append(cmd.Env, command.Env...)

	cmd.Stdout = info
	cmd.Stderr = errors
	// Run the command
	err := cmd.Run()
	if err != nil {
		runIndex.Store(jobId, false)
		errors.SetText(fmt.Sprintf("error running command: %v", err.Error()))
		exitCode.SetText(fmt.Sprintf("[red] Exit code: %v", cmd.ProcessState.ExitCode()))
		app.QueueUpdateDraw(func() {})
		return
	}
	runIndex.Store(jobId, false)

	exitCode.SetText(fmt.Sprintf("[green] Exit code: %v", cmd.ProcessState.ExitCode()))
	app.QueueUpdateDraw(func() {})
}

func FuzzyFind(input string, commands []Command) []string {
	commandList := []string{}
	for _, c := range commands {
		commandList = append(commandList, c.Label)
	}
	return fuzzy.Find(input, commandList)
}

type Command struct {
	Color   tcell.Color
	Label   string
	Command string
	Args    []string
	Env     []string
}

type ConfigCommand struct {
	Label   string   `yaml:"label"`
	Command string   `yaml:"command"`
	Shell   string   `yaml:"shell"`
	Args    string   `yaml:"args"`
	Env     []string `yaml:"env"`
}

func getFilteredList(l *tview.List, commands []Command, filterString string) {
	if l != nil {
		l.Clear()
	}

	filteredCommands := FuzzyFind(filterString, commands)

	filteredResults = []string{}

	for i := range commands {
		c := &(commands[i])
		matchedMarker := ""
		if !slices.Contains(filteredCommands, c.Label) {
			continue
		}

		l.AddItem(fmt.Sprintf("%v%v", (*c).Label, matchedMarker), "", 0, func() { go runCommand(c) }).ShowSecondaryText(false) // .SetMainTextColor(c.Color)
		filteredResults = append(filteredResults, (*c).Label)
	}

	l.SetBorder(true)
}

func getLayout(commands []Command) {
	getFilteredList(list, commands, searchTerm)

	info = tview.NewTextView().SetTextAlign(tview.AlignLeft).SetText("").SetDynamicColors(true)
	errors = tview.NewTextView().SetTextAlign(tview.AlignLeft).SetText("").SetDynamicColors(true)
	exitCode = tview.NewTextView().SetTextAlign(tview.AlignLeft).SetText("").SetDynamicColors(true)
	bottomLeftText = tview.NewTextView().SetTextAlign(tview.AlignLeft).SetDynamicColors(true)
	bottomLeftSearch = tview.NewInputField()

	setBottomLeftText("0 processes running")

	info.SetBorder(true).SetTitle("stdout")
	errors.SetBorder(true).SetTitle("stderr")
	exitCode.SetBorder(true).SetTitle("exit code")
	bottomLeftText.SetBorder(true)
	bottomLeftSearch.SetBorder(true)
	bottomLeftSearch.SetDisabled(true)

	logViews := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(info, 0, 5, false).
		AddItem(errors, 0, 5, false)
		// AddItem(exitCode, 0, 1, false)

	mainColumns := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(list, 0, 1, true).
		AddItem(logViews, 0, 2, false)

	bottomRow := tview.NewFlex().SetDirection(tview.FlexColumn)

	bottomLeftFlex := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(bottomLeftText, 0, 1, false).
		AddItem(bottomLeftSearch, 0, 1, false)

	bottomRow.AddItem(bottomLeftFlex, 0, 2, false).
		AddItem(exitCode, 0, 1, false)

	layout = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(mainColumns, 0, 1, false).
		AddItem(bottomRow, 3, 0, false)
}

func endSearch(msg string) {
	isSearching = false
	searchTerm = ""
	bottomLeftSearch.SetDisabled(true)
	app.SetFocus(list)
	setBottomLeftText(msg)
}

func main() {
	runIndex = sync.Map{}
	var err error

	config, err = loadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err.Error())
	}

	parseConfigCommands(config)

	app = tview.NewApplication()

	go pidRunningDrawLoop()

	list = tview.NewList()
	getLayout(*commands)

	currentlyFocusedBox = BOX_LIST

	app.SetFocus(list)

	app.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Rune() == '/' {
			if !isSearching {
				isSearching = true
				searchTerm = ""
				// app.SetFocus(bottomLeftSearch)
				// getLayout(commands)
				setBottomLeftText("[aqua]searching:")
				bottomLeftSearch.SetText("")
				bottomLeftSearch.SetDisabled(false)
				// searchTerm = fmt.Sprintf("%v%v", searchTerm, string(e.Rune()))
				app.SetFocus(bottomLeftSearch)
				bottomLeftSearch.SetChangedFunc(func(text string) {
					if list != nil {
						list.Clear()
					}
					searchTerm = text
					getFilteredList(list, *commands, searchTerm)
				})
				return nil
			}
		} else if e.Key() == tcell.KeyEnter {
			if isSearching {
				if len(filteredResults) == 0 {
					getFilteredList(list, *commands, "")
				}
				endSearch(fmt.Sprintf("searched: %v", searchTerm))
				return nil
			}
		} else if e.Key() == tcell.KeyEscape {
			if isSearching {
				endSearch(fmt.Sprintf("canceled search: %v", searchTerm))
				return nil
			} else {
				app.Stop()
			}
		} else if e.Key() == tcell.KeyLeft {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(list)
				currentlyFocusedBox = BOX_LIST
			case BOX_STDERR:
				app.SetFocus(list)
				currentlyFocusedBox = BOX_LIST
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(info)
				currentlyFocusedBox = BOX_STDOUT
			}
			return nil
		} else if e.Key() == tcell.KeyRight {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(list)
				currentlyFocusedBox = BOX_LIST
			case BOX_STDERR:
				app.SetFocus(list)
				currentlyFocusedBox = BOX_LIST
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(info)
				currentlyFocusedBox = BOX_STDOUT
			}
			return nil
		} else if e.Key() == tcell.KeyTab {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(errors)
				currentlyFocusedBox = BOX_STDERR
			case BOX_STDERR:
				app.SetFocus(list)
				currentlyFocusedBox = BOX_LIST
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(info)
				currentlyFocusedBox = BOX_STDOUT
			}
			return nil
		} else if e.Key() == tcell.KeyBacktab {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(list)
				currentlyFocusedBox = BOX_LIST
			case BOX_STDERR:
				app.SetFocus(info)
				currentlyFocusedBox = BOX_STDOUT
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(errors)
				currentlyFocusedBox = BOX_STDERR
			}
			return nil
		} else if e.Key() == tcell.KeyPgUp {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(info)
				r, c := info.GetScrollOffset()
				_, _, _, h := info.GetRect()
				newRow := r - h + 2 // the borders add some extra distance
				if newRow < 0 {
					newRow = 0
				}
				info.ScrollTo(newRow, c)
				return nil
			case BOX_STDERR:
				app.SetFocus(errors)
				r, c := errors.GetScrollOffset()
				_, _, _, h := errors.GetRect()
				newRow := r - h + 2 // the borders add some extra distance
				if newRow < 0 {
					newRow = 0
				}
				errors.ScrollTo(newRow, c)
				return nil
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(list)
				return e
			}
		} else if e.Key() == tcell.KeyPgDn {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(info)
				r, c := info.GetScrollOffset()
				_, _, _, h := info.GetRect()
				newRow := r + h - 2 // the borders add some extra distance
				info.ScrollTo(newRow, c)
				return nil
			case BOX_STDERR:
				app.SetFocus(errors)
				r, c := info.GetScrollOffset()
				_, _, _, h := errors.GetRect()
				newRow := r + h - 2 // the borders add some extra distance
				errors.ScrollTo(newRow, c)
				return nil
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(list)
				return e
			}
		} else if e.Key() == tcell.KeyUp {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(info)
				r, c := info.GetScrollOffset()
				newRow := r - 1
				info.ScrollTo(newRow, c)
				return nil
			case BOX_STDERR:
				app.SetFocus(errors)
				r, c := errors.GetScrollOffset()
				newRow := r - 1
				errors.ScrollTo(newRow, c)
				return nil
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(list)
				return e
			}
		} else if e.Key() == tcell.KeyDown {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				app.SetFocus(info)
				r, c := info.GetScrollOffset()
				newRow := r + 1
				info.ScrollTo(newRow, c)
				return nil
			case BOX_STDERR:
				app.SetFocus(errors)
				r, c := info.GetScrollOffset()
				newRow := r + 1
				errors.ScrollTo(newRow, c)
				return nil
			case BOX_LIST:
				fallthrough
			default:
				app.SetFocus(list)
				return e
			}
		} else if e.Key() == tcell.KeyHome {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				info.ScrollToBeginning()
				return nil
			case BOX_STDERR:
				errors.ScrollToBeginning()
				return nil
			case BOX_LIST:
				fallthrough
			default:
				return e
			}
		} else if e.Key() == tcell.KeyEnd {
			if isSearching {
				return e
			}
			switch currentlyFocusedBox {
			case BOX_STDOUT:
				info.ScrollToEnd()
				return nil
			case BOX_STDERR:
				errors.ScrollToEnd()
				return nil
			case BOX_LIST:
				fallthrough
			default:
				return e
			}
		} else {
			// if isSearching {
			// 	setBottomLeftText("[aqua]searching:")
			// 	bottomLeftSearch.SetDisabled(false)
			// 	bottomLeftSearch.Focus(func(p tview.Primitive) {})
			// 	searchTerm = fmt.Sprintf("%v%v", searchTerm, string(e.Rune()))
			// 	bottomLeftSearch.SetChangedFunc(func(text string) {
			// 		if list != nil {
			// 			list.Clear()
			// 		}
			// 		searchTerm = text
			// 		getFilteredList(list, commands, searchTerm)
			// 		bottomLeftSearch.SetText(searchTerm)
			// 		bottomLeftSearch.Focus(func(p tview.Primitive) {})
			// 	})
			// 	return nil
			// }
			if !isSearching {
				app.SetFocus(list)
			}
		}
		return e
	})

	if err := app.SetRoot(layout, true).EnableMouse(false).Run(); err != nil {
		panic(err)
	}
}
