package main

import (
	"spectrum"
	"exp/eval"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"container/vector"
	"⚛readline"
	"sync"
	"bytes"
	"time"
)


// ==============
// Some variables
// ==============

// These variables are set only once, before starting new goroutines,
// so there is no need for controlling concurrent access via a sync.Mutex
var app *spectrum.Application
var speccy *spectrum.Spectrum48k
var w *eval.World

const PROMPT = "gospeccy> "
const PROMPT_EMPTY = "          "

// Whether the terminal is currently showing a prompt string
var havePrompt = false
var havePrompt_mutex sync.Mutex

const SCRIPT_DIRECTORY = "scripts"
const STARTUP_SCRIPT = "startup"


// ================
// Various commands
// ================

var help_keys vector.StringVector
var help_vals vector.StringVector

func printHelp() {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "\nAvailable commands:\n")

	maxKeyLen := 1
	for i := 0; i < help_keys.Len(); i++ {
		if len(help_keys[i]) > maxKeyLen {
			maxKeyLen = len(help_keys[i])
		}
	}

	for i := 0; i < help_keys.Len(); i++ {
		fmt.Fprintf(&buf, "    %s", help_keys[i])
		for j := len(help_keys[i]); j < maxKeyLen; j++ {
			fmt.Fprintf(&buf, " ")
		}
		fmt.Fprintf(&buf, "  %s\n", help_vals[i])
	}

	app.PrintfMsg("%s\n", buf.String())
}

// Signature: func help()
func wrapper_help(t *eval.Thread, in []eval.Value, out []eval.Value) {
	printHelp()
}

// Signature: func exit()
func wrapper_exit(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	app.RequestExit()
}

// Signature: func reset()
func wrapper_reset(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	speccy.CommandChannel <- spectrum.Cmd_Reset{}
}

// Signature: func load(path string)
func wrapper_load(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	path := in[0].(eval.StringValue).Get(t)

	data, err := ioutil.ReadFile(spectrum.SnaPath(path))
	if err != nil {
		app.PrintfMsg("%s", err)
		return
	}

	errChan := make(chan os.Error)
	speccy.CommandChannel <- spectrum.Cmd_LoadSna{path, data, errChan}
	err = <-errChan
	if err != nil {
		app.PrintfMsg("%s", err)
	}
}

// Signature: func save(path string)
func wrapper_save(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	path := in[0].(eval.StringValue).Get(t)

	ch := make(chan spectrum.Snapshot)
	speccy.CommandChannel <- spectrum.Cmd_SaveSna{ch}

	var snapshot spectrum.Snapshot = <-ch
	if snapshot.Err != nil {
		app.PrintfMsg("%s", snapshot.Err)
		return
	}

	err := ioutil.WriteFile(path, snapshot.Data, 0600)
	if err != nil {
		app.PrintfMsg("%s", err)
	}

	if app.Verbose {
		app.PrintfMsg("wrote SNA snapshot \"%s\"", path)
	}
}

// Signature: func scale(n uint)
func wrapper_scale(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	n := in[0].(eval.UintValue).Get(t)

	switch n {
	case 1:
		finished := make(chan byte)
		speccy.CommandChannel <- spectrum.Cmd_CloseAllDisplays{finished}
		<-finished

		speccy.CommandChannel <- spectrum.Cmd_AddDisplay{spectrum.NewSDLScreen(app)}

	case 2:
		finished := make(chan byte)
		speccy.CommandChannel <- spectrum.Cmd_CloseAllDisplays{finished}
		<-finished

		speccy.CommandChannel <- spectrum.Cmd_AddDisplay{spectrum.NewSDLScreen2x(app, /*fullscreen*/ false)}
	}
}

// Signature: func fps(n float)
func wrapper_fps(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	fps := in[0].(eval.FloatValue).Get(t)
	speccy.CommandChannel <- spectrum.Cmd_SetFPS{float(fps)}
}

// Signature: func ULA_accuracy(accurateEmulation bool)
func wrapper_ulaAccuracy(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	accurateEmulation := in[0].(eval.BoolValue).Get(t)
	speccy.CommandChannel <- spectrum.Cmd_SetUlaEmulationAccuracy{accurateEmulation}
}

// Signature: func sound(enable bool)
func wrapper_sound(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	enable := in[0].(eval.BoolValue).Get(t)

	if enable {
		audio, err := spectrum.NewSDLAudio(app)
		if err == nil {
			finished := make(chan byte)
			speccy.CommandChannel <- spectrum.Cmd_CloseAllAudioReceivers{finished}
			<-finished

			speccy.CommandChannel <- spectrum.Cmd_AddAudioReceiver{audio}
		} else {
			app.PrintfMsg("%s", err)
		}
	} else {
		finished := make(chan byte)
		speccy.CommandChannel <- spectrum.Cmd_CloseAllAudioReceivers{finished}
		<-finished
	}
}

// Signature: func wait(milliseconds uint)
func wrapper_wait(t *eval.Thread, in []eval.Value, out []eval.Value) {
	if app.TerminationInProgress() {
		return
	}

	milliseconds := uint(in[0].(eval.UintValue).Get(t))
	time.Sleep(1e6 * int64(milliseconds))
}

// Signature: func script(scriptName string)
func wrapper_script(t *eval.Thread, in []eval.Value, out []eval.Value) {
	scriptName := in[0].(eval.StringValue).Get(t)

	err := runScript(w, scriptName, /*optional*/ false)
	if err != nil {
		app.PrintfMsg("%s", err)
		return
	}
}

// Signature: func optionalScript(scriptName string)
func wrapper_optionalScript(t *eval.Thread, in []eval.Value, out []eval.Value) {
	scriptName := in[0].(eval.StringValue).Get(t)

	err := runScript(w, scriptName, /*optional*/ true)
	if err != nil {
		app.PrintfMsg("%s", err)
		return
	}
}


// ==============
// Initialization
// ==============

func defineFunctions(w *eval.World) {
	{
		var functionSignature func()
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_help, functionSignature)
		w.DefineVar("help", funcType, funcValue)
		help_keys.Push("help()")
		help_vals.Push("This help")
	}

	{
		var functionSignature func()
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_exit, functionSignature)
		w.DefineVar("exit", funcType, funcValue)
		help_keys.Push("exit()")
		help_vals.Push("Terminate this program")
	}

	{
		var functionSignature func()
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_reset, functionSignature)
		w.DefineVar("reset", funcType, funcValue)
		help_keys.Push("reset()")
		help_vals.Push("Reset the emulated machine")
	}

	{
		var functionSignature func(string)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_load, functionSignature)
		w.DefineVar("load", funcType, funcValue)
		help_keys.Push("load(path string)")
		help_vals.Push("Load state from file (SNA format)")
	}

	{
		var functionSignature func(string)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_save, functionSignature)
		w.DefineVar("save", funcType, funcValue)
		help_keys.Push("save(path string)")
		help_vals.Push("Save state to file (SNA format)")
	}

	{
		var functionSignature func(uint)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_scale, functionSignature)
		w.DefineVar("scale", funcType, funcValue)
		help_keys.Push("scale(n uint)")
		help_vals.Push("Change the display scale")
	}

	{
		var functionSignature func(float)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_fps, functionSignature)
		w.DefineVar("fps", funcType, funcValue)
		help_keys.Push("fps(n float)")
		help_vals.Push("Change the display refresh frequency")
	}

	{
		var functionSignature func(bool)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_ulaAccuracy, functionSignature)
		w.DefineVar("ULA_accuracy", funcType, funcValue)
		help_keys.Push("ULA_accuracy(accurateEmulation bool)")
		help_vals.Push("Enable/disable accurate emulation of screen bitmap and screen attributes")
	}

	{
		var functionSignature func(bool)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_sound, functionSignature)
		w.DefineVar("sound", funcType, funcValue)
		help_keys.Push("sound(enable bool)")
		help_vals.Push("Enable or disable sound")
	}

	{
		var functionSignature func(uint)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_wait, functionSignature)
		w.DefineVar("wait", funcType, funcValue)
		help_keys.Push("wait(milliseconds uint)")
		help_vals.Push("Wait the specified amount of time before issuing the next command")
	}

	{
		var functionSignature func(string)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_script, functionSignature)
		w.DefineVar("script", funcType, funcValue)
		help_keys.Push("script(scriptName string)")
		help_vals.Push("Load and evaluate the specified Go script")
	}

	{
		var functionSignature func(string)
		funcType, funcValue := eval.FuncFromNativeTyped(wrapper_optionalScript, functionSignature)
		w.DefineVar("optionalScript", funcType, funcValue)
		help_keys.Push("optionalScript(scriptName string)")
		help_vals.Push("Load (if found) and evaluate the specified Go script")
	}
}


// Runs the specified Go source code in the context of 'w'
func run(w *eval.World, sourceCode string) {
	// Avoids the need to put ";" at the end of the code
	sourceCode = sourceCode + "\n"

	var err os.Error

	var code eval.Code
	code, err = w.Compile(sourceCode)
	if err != nil {
		app.PrintfMsg("%s", err)
		return
	}

	_, err = code.Run()
	if err != nil {
		app.PrintfMsg("%s", err)
		return
	}
}

// Loads and evaluates the specified Go script
func runScript(w *eval.World, scriptName string, optional bool) os.Error {
	fileName := scriptName + ".go"
	data, err := ioutil.ReadFile(spectrum.ScriptPath(fileName))
	if err != nil {
		if !optional {
			return err
		} else {
			return nil
		}
	}

	var buf bytes.Buffer
	buf.Write(data)
	run(w, buf.String())

	return nil
}


type handler_t byte

func (h handler_t) HandleSignal(s signal.Signal) {
	switch ss := s.(type) {
	case signal.UnixSignal:
		switch ss {
		case signal.SIGQUIT, signal.SIGTERM, signal.SIGALRM, signal.SIGTSTP, signal.SIGTTIN, signal.SIGTTOU:
			readline.CleanupAfterSignal()

		case signal.SIGINT:
			readline.FreeLineState()
			readline.CleanupAfterSignal()

		case signal.SIGWINCH:
			readline.ResizeTerminal()
		}
	}
}

// Reads lines from os.Stdin and sends them through the channel 'code'.
//
// If no more input is available, an arbitrary value is sent through channel 'no_more_code'.
//
// This function is intended to be run in a separate goroutine.
func readCode(app *spectrum.Application, code chan string, no_more_code chan<- byte) {
	handler := handler_t(0)
	spectrum.InstallSignalHandler(handler)

	// BNF pattern: (string address)* nil
	readline_channel := make(chan *string)
	go func() {
		prevMsgOut := app.SetMessageOutput(&consoleMessageOutput{})

		for {
			havePrompt_mutex.Lock()
			havePrompt = true
			havePrompt_mutex.Unlock()

			if app.TerminationInProgress() {
				break
			}

			line := readline.ReadLine(PROMPT)

			havePrompt_mutex.Lock()
			havePrompt = false
			havePrompt_mutex.Unlock()

			readline_channel <- line
			if line == nil {
				break
			} else {
				<-readline_channel
			}
		}

		app.SetMessageOutput(prevMsgOut)
	}()

	evtLoop := app.NewEventLoop()
	for {
		select {
		case <-evtLoop.Pause:
			spectrum.UninstallSignalHandler(handler)

			havePrompt_mutex.Lock()
			if havePrompt && len(PROMPT) > 0 {
				fmt.Printf("\r%s\r", PROMPT_EMPTY)
				havePrompt = false
			}
			havePrompt_mutex.Unlock()

			readline.FreeLineState()
			readline.CleanupAfterSignal()

			evtLoop.Pause <- 0

		case <-evtLoop.Terminate:
			if evtLoop.App().Verbose {
				app.PrintfMsg("readCode loop: exit")
			}
			evtLoop.Terminate <- 0
			return

		case lineP := <-readline_channel:
			// EOF
			if lineP == nil {
				no_more_code <- 0
				evtLoop.Delete()
				continue
			}

			line := strings.TrimSpace(*lineP)

			if len(line) > 0 {
				readline.AddHistory(line)
			}

			code <- line
			<-code

			readline_channel <- nil
		}
	}
}


// Reads lines of Go code from standard input and evaluates the code.
//
// This function exits in two cases: if the application was terminated (from outside of this function),
// or if there is nothing more to read from os.Stdin. The latter can optionally cause the whole application
// to terminate (controlled by the 'exitAppIfEndOfInput' parameter).
func runConsole(_app *spectrum.Application, _speccy *spectrum.Spectrum48k, exitAppIfEndOfInput bool) {
	if app != nil {
		panic("running multiple consoles is unsupported")
	}

	app = _app
	speccy = _speccy
	w = eval.NewWorld()

	defineFunctions(w)

	// Run the startup script
	{
		var err os.Error
		err = runScript(w, STARTUP_SCRIPT, /*optional*/ false)
		if err != nil {
			app.PrintfMsg("%s", err)
			app.RequestExit()
			return
		}

		if app.TerminationInProgress() || closed(app.HasTerminated) {
			return
		}
	}

	// This should be printed before executing "go readCode(...)",
	// in order to ensure that this message *always* gets printed before printing the prompt
	app.PrintfMsg("Hint: Input an empty line to see available commands")

	// Start a goroutine for reading code from os.Stdin.
	// The code pieces are being received from the channel 'code_chan'.
	code_chan := make(chan string)
	no_more_code := make(chan byte)
	go readCode(app, code_chan, no_more_code)

	// Loop pattern: (read code, run code)+ (terminate app)?
	evtLoop := app.NewEventLoop()
	for {
		select {
		case <-evtLoop.Pause:
			evtLoop.Pause <- 0

		case <-evtLoop.Terminate:
			// Exit this function
			if app.Verbose {
				app.PrintfMsg("console loop: exit")
			}
			evtLoop.Terminate <- 0
			return

		case code := <-code_chan:
			//app.PrintfMsg("code=\"%s\"", code)
			if len(code) > 0 {
				run(w, code)
			} else {
				printHelp()
			}
			code_chan <- "<next>"

		case <-no_more_code:
			if exitAppIfEndOfInput {
				app.RequestExit()
			} else {
				evtLoop.Delete()
			}
		}
	}
}


type consoleMessageOutput struct{
	// This mutex is used to serialize the multiple calls to fmt.Printf
	// used in function PrintfMsg. Otherwise, a concurrent entry to PrintfMsg
	// would cause undesired interleaving of fmt.Printf calls.
	mutex sync.Mutex
}

// Prints a single-line message to 'os.Stdout' using 'fmt.Printf'.
// If the format string does not end with the new-line character,
// the new-line character is appended automatically.
//
// Using this function instead of 'fmt.Printf', 'println', etc,
// ensures proper redisplay of the current command line.
func (out *consoleMessageOutput) PrintfMsg(format string, a ...interface{}) {
	out.mutex.Lock()
	{
		havePrompt_mutex.Lock()
		if havePrompt && len(PROMPT) > 0 {
			fmt.Printf("\r%s\r", PROMPT_EMPTY)
		}
		havePrompt_mutex.Unlock()

		appendNewLine := false
		if (len(format) == 0) || (format[len(format)-1] != '\n') {
			appendNewLine = true
		}

		fmt.Printf(format, a)
		if appendNewLine {
			fmt.Println()
		}

		havePrompt_mutex.Lock()
		if havePrompt {
			if (app == nil) || !app.TerminationInProgress() {
				readline.OnNewLine()
				readline.Redisplay()
				// 'havePrompt' remains to have the value 'true'
			} else {
				havePrompt = false
			}
		}
		havePrompt_mutex.Unlock()
	}
	out.mutex.Unlock()
}