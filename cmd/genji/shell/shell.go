package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/c-bata/go-prompt"
	"github.com/dgraph-io/badger/v2"
	"github.com/genjidb/genji"
	"github.com/genjidb/genji/document"
	"github.com/genjidb/genji/engine"
	"github.com/genjidb/genji/engine/badgerengine"
	"github.com/genjidb/genji/engine/boltengine"
	"github.com/genjidb/genji/engine/memoryengine"
	"github.com/genjidb/genji/sql/parser"
	"github.com/hashicorp/go-multierror"
	"golang.org/x/sync/errgroup"
)

const (
	historyFilename = ".genji_history"
)

var (
	// error returned when the exit command is executed
	errExitCommand = errors.New("exit command")
	// error returned when the program received a termination signal
	errExitSignal = errors.New("termination signal received")
	// error returned when the prompt reads a ctrl d input
	errExitCtrlD = errors.New("ctrl-d")
)

// A Shell manages a command line shell program for manipulating a Genji database.
type Shell struct {
	db   *genji.DB
	opts *Options

	query      string
	livePrefix string
	multiLine  bool

	history []string

	cmdSuggestions []prompt.Suggest

	// context used for execution cancelation
	execContext  context.Context
	execCancelFn func()
	mu           sync.Mutex
}

// Options of the shell.
type Options struct {
	// Name of the engine to use when opening the database.
	// Must be either "memory", "bolt" or "badger"
	// If empty, "memory" will be used, unless DBPath is non empty.
	// In that case "bolt" will be used.
	Engine string
	// Path of the database file or directory that will be created.
	DBPath string
}

func (o *Options) validate() error {
	if o.Engine == "" {
		if o.DBPath == "" {
			o.Engine = "memory"
		} else {
			o.Engine = "bolt"
		}
	}

	switch o.Engine {
	case "bolt", "badger", "memory":
	default:
		return fmt.Errorf("unsupported engine %q", o.Engine)
	}

	return nil
}

func stdinFromTerminal() bool {
	fi, _ := os.Stdin.Stat()
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return false // data is from pipe
	}
	return true // data is from terminal
}

// Run a shell.
func Run(ctx context.Context, opts *Options) (err error) {
	if opts == nil {
		opts = new(Options)
	}

	err = opts.validate()
	if err != nil {
		return err
	}

	var sh Shell

	sh.opts = opts

	if stdinFromTerminal() {
		switch opts.Engine {
		case "memory":
			fmt.Println("Opened an in-memory database.")
		case "bolt":
			fmt.Printf("On-disk database using BoltDB engine at path %s.\n", opts.DBPath)
		case "badger":
			fmt.Printf("On-disk database using Badger engine at path %s.\n", opts.DBPath)
		}
		fmt.Println("Enter \".help\" for usage hints.")
	}

	ran, err := sh.runPipedInput(ctx)
	if err != nil {
		return err
	}
	if ran {
		return nil
	}

	defer func() {
		if sh.db != nil {
			closeErr := sh.db.Close()
			if closeErr != nil {
				err = multierror.Append(err, closeErr)
			}
		}
	}()

	defer func() {
		dumpErr := sh.dumpHistory()
		if dumpErr != nil {
			err = multierror.Append(err, dumpErr)
		}
	}()

	promptExecCh := make(chan string)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		interruptC := make(chan os.Signal, 1)
		termC := make(chan os.Signal, 1)
		signal.Notify(interruptC, os.Interrupt)
		signal.Notify(termC, syscall.SIGTERM)

		for {
			select {
			case <-interruptC:
				sh.cancelExecution()
			case <-termC:
				fmt.Fprintf(os.Stderr, "\nTermination signal received. Quitting...\n")
				return errExitSignal
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case input := <-promptExecCh:
				err := sh.executeInput(sh.getExecContext(ctx), input)
				// if the context has been canceled
				// there is no way to tell at this point
				// if this is because of a user interruption
				// or a termination signal.
				// If it's the latter, it will be detected by the Select statement.
				if err == context.Canceled {
					// Print a newline for cleanliness
					fmt.Println()
					continue
				}
				if err == errExitCommand {
					return err
				}
				if err != nil {
					fmt.Println(err)
				}
			case promptExecCh <- "":
			}
		}
	})

	go func() {
		defer cancel()

		err := sh.runPrompt(ctx, promptExecCh)
		if err != nil && err != errExitCtrlD {
			fmt.Fprintf(os.Stderr, err.Error())
		}
	}()

	err = g.Wait()
	if err == errExitCommand || err == errExitSignal || err == context.Canceled {
		return nil
	}

	return err
}

func (sh *Shell) runPrompt(ctx context.Context, execCh chan (string)) error {
	sh.loadCommandSuggestions()
	history, err := sh.loadHistory()
	if err != nil {
		return err
	}

	var lastKeyStroke prompt.Key

	promptOpts := []prompt.Option{
		prompt.OptionPrefix("genji> "),
		prompt.OptionTitle("genji"),
		prompt.OptionLivePrefix(sh.changelivePrefix),
		prompt.OptionHistory(history),
		prompt.OptionBreakLineCallback(func(d *prompt.Document) {
			lastKeyStroke = d.LastKeyStroke()
		}),
	}

	// If NO_COLOR env var is present, disable color. See https://no-color.org
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		// A list of color options we have to reset.
		colorOpts := []func(prompt.Color) prompt.Option{
			prompt.OptionPrefixTextColor,
			prompt.OptionPreviewSuggestionTextColor,
			prompt.OptionSuggestionTextColor,
			prompt.OptionSuggestionBGColor,
			prompt.OptionSelectedSuggestionTextColor,
			prompt.OptionSelectedSuggestionBGColor,
			prompt.OptionDescriptionTextColor,
			prompt.OptionDescriptionBGColor,
			prompt.OptionSelectedDescriptionTextColor,
			prompt.OptionSelectedDescriptionBGColor,
			prompt.OptionScrollbarThumbColor,
			prompt.OptionScrollbarBGColor,
		}
		for _, opt := range colorOpts {
			resetColor := opt(prompt.DefaultColor)
			promptOpts = append(promptOpts, resetColor)
		}
	}

	pt := prompt.New(func(in string) {}, sh.completer, append(promptOpts, prompt.OptionHistory(history))...)

	for {
		input := pt.Input()

		if lastKeyStroke == prompt.ControlD {
			return errExitCtrlD
		}

		input = strings.TrimSpace(input)

		if len(input) == 0 {
			continue
		}

		execCh <- input
		<-execCh
	}
}

func (sh *Shell) cancelExecution() {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.execCancelFn != nil {
		sh.execCancelFn()
		sh.execContext = nil
		sh.execCancelFn = nil
	}
}

func (sh *Shell) getExecContext(ctx context.Context) context.Context {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.execContext != nil {
		return sh.execContext
	}

	sh.execContext, sh.execCancelFn = context.WithCancel(ctx)
	return sh.execContext
}

func (sh *Shell) loadCommandSuggestions() {
	suggestions := make([]prompt.Suggest, 0, len(commands))
	for _, c := range commands {
		suggestions = append(suggestions, prompt.Suggest{
			Text: c.Name,
		})

		for _, alias := range c.Aliases {
			suggestions = append(suggestions, prompt.Suggest{
				Text: alias,
			})
		}
	}
	sh.cmdSuggestions = suggestions
}

func (sh *Shell) execute(ctx context.Context, in string) {
	sh.history = append(sh.history, in)

	err := sh.executeInput(ctx, in)
	if err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (sh *Shell) executeInput(ctx context.Context, in string) error {
	sh.history = append(sh.history, in)

	switch {
	// if it starts with a "." it's a command
	// if the input is "help" or "exit", then it's a command.
	// it must not be in the middle of a multi line query though
	case !sh.multiLine && strings.HasPrefix(in, "."), in == "help", in == "exit":
		return sh.runCommand(ctx, in)

	// If it ends with a ";" we can run a query
	case strings.HasSuffix(in, ";"):
		sh.query = sh.query + in
		sh.multiLine = false
		sh.livePrefix = in
		err := sh.runQuery(ctx, sh.query)
		sh.query = ""
		return err
	// If the input is empty we ignore it
	case in == "":
		return nil

	// If we reach this case, it means the user is in the middle of a
	// multi line query. We change the prompt and set the multiLine var to true.
	default:
		sh.query = sh.query + in + " "
		sh.livePrefix = "... "
		sh.multiLine = true
	}

	return nil
}

func (sh *Shell) runCommand(ctx context.Context, in string) error {
	in = strings.TrimSuffix(in, ";")
	cmd := strings.Fields(in)
	switch cmd[0] {
	case ".help", "help":
		return runHelpCmd()
	case ".tables":
		db, err := sh.getDB()
		if err != nil {
			return err
		}

		return runTablesCmd(db, cmd)
	case ".exit", "exit":
		if len(cmd) > 1 {
			return fmt.Errorf("usage: .exit")
		}

		return errExitCommand
	case ".indexes":
		db, err := sh.getDB()
		if err != nil {
			return err
		}
		return runIndexesCmd(db, cmd)
	case ".dump":
		db, err := sh.getDB()
		if err != nil {
			return err
		}

		return runDumpCmd(db, cmd[1:], os.Stdout)
	default:
		return displaySuggestions(in)
	}
}

func (sh *Shell) runQuery(ctx context.Context, q string) error {
	db, err := sh.getDB()
	if err != nil {
		return err
	}

	res, err := db.Query(ctx, q)
	if err != nil {
		return err
	}

	defer res.Close()

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return res.Iterate(func(d document.Document) error {
		select {
		case <-ctx.Done():
			return errors.New("interrupted")
		default:
		}

		return enc.Encode(d)
	})
}

func (sh *Shell) getDB() (*genji.DB, error) {
	if sh.db != nil {
		return sh.db, nil
	}

	var ng engine.Engine
	var err error

	switch sh.opts.Engine {
	case "memory":
		ng = memoryengine.NewEngine()
	case "bolt":
		ng, err = boltengine.NewEngine(sh.opts.DBPath, 0660, nil)
	case "badger":
		ng, err = badgerengine.NewEngine(badger.DefaultOptions(sh.opts.DBPath).WithLogger(nil))
	}
	if err != nil {
		return nil, err
	}

	sh.db, err = genji.New(ng)
	if err != nil {
		return nil, err
	}

	return sh.db, nil
}

func (sh *Shell) runPipedInput(ctx context.Context) (ran bool, err error) {
	// Check if there is any input being piped in from the terminal
	stat, _ := os.Stdin.Stat()
	m := stat.Mode()

	if (m&os.ModeNamedPipe) == 0 /*cat a.txt| prog*/ && !m.IsRegular() /*prog < a.txt*/ { // No input from terminal
		return false, nil
	}
	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		return true, fmt.Errorf("Unable to read piped input: %w", err)
	}
	err = sh.runQuery(ctx, string(data))
	if err != nil {
		return true, fmt.Errorf("Unable to execute provided sql statements: %w", err)
	}

	return true, nil
}

func (sh *Shell) changelivePrefix() (string, bool) {
	return sh.livePrefix, sh.multiLine
}

func (sh *Shell) getAllIndexes() ([]string, error) {
	db, err := sh.getDB()
	if err != nil {
		return nil, err
	}

	var listName []string
	err = db.View(func(tx *genji.Tx) error {
		indexes, err := tx.ListIndexes()
		if err != nil {
			return err
		}

		for _, idx := range indexes {
			listName = append(listName, idx.IndexName)
		}

		return nil
	})

	if len(listName) == 0 {
		listName = append(listName, "index_name")
	}

	return listName, err
}

// getTables returns all the tables of the database
func (sh *Shell) getAllTables() ([]string, error) {
	var tables []string
	db, _ := sh.getDB()
	res, err := db.Query(context.Background(), "SELECT table_name FROM __genji_tables")
	if err != nil {
		return nil, err
	}
	defer res.Close()

	err = res.Iterate(func(d document.Document) error {
		var tableName string
		err = document.Scan(d, &tableName)
		if err != nil {
			return err
		}
		tables = append(tables, tableName)
		return nil
	})

	// if there is no table return table as a suggestion
	if len(tables) == 0 {
		tables = append(tables, "table_name")
	}

	return tables, nil
}

func (sh *Shell) completer(in prompt.Document) []prompt.Suggest {
	suggestions := prompt.FilterHasPrefix(sh.cmdSuggestions, in.Text, true)

	_, err := parser.NewParser(strings.NewReader(in.Text)).ParseQuery(context.Background())
	if err != nil {
		e, ok := err.(*parser.ParseError)
		if !ok || len(e.Expected) < 1 {
			return suggestions
		}
		expected := e.Expected
		switch expected[0] {
		case "table_name":
			expected, err = sh.getAllTables()
			if err != nil {
				return suggestions
			}
		case "index_name":
			expected, err = sh.getAllIndexes()
			if err != nil {
				return suggestions
			}
		}
		for _, e := range expected {
			suggestions = append(suggestions, prompt.Suggest{
				Text: e,
			})
		}

		w := in.GetWordBeforeCursor()
		if w == "" {
			return suggestions
		}

		return prompt.FilterHasPrefix(suggestions, w, true)
	}

	return []prompt.Suggest{}
}
