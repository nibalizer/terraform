package command

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/terraform"
	"github.com/mitchellh/cli"
	"github.com/mitchellh/colorstring"
)

// Meta are the meta-options that are available on all or most commands.
type Meta struct {
	Color       bool
	ContextOpts *terraform.ContextOpts
	Ui          cli.Ui

	// State read when calling `Context`. This is available after calling
	// `Context`.
	state *terraform.State

	// This can be set by the command itself to provide extra hooks.
	extraHooks []terraform.Hook

	// Variables for the context (private)
	autoKey       string
	autoVariables map[string]string
	input         bool
	variables     map[string]string

	color bool
	oldUi cli.Ui
}

// Colorize returns the colorization structure for a command.
func (m *Meta) Colorize() *colorstring.Colorize {
	return &colorstring.Colorize{
		Colors:  colorstring.DefaultColors,
		Disable: !m.color,
		Reset:   true,
	}
}

// Context returns a Terraform Context taking into account the context
// options used to initialize this meta configuration.
func (m *Meta) Context(copts contextOpts) (*terraform.Context, bool, error) {
	opts := m.contextOpts()

	// First try to just read the plan directly from the path given.
	f, err := os.Open(copts.Path)
	if err == nil {
		plan, err := terraform.ReadPlan(f)
		f.Close()
		if err == nil {
			if len(m.variables) > 0 {
				return nil, false, fmt.Errorf(
					"You can't set variables with the '-var' or '-var-file' flag\n" +
						"when you're applying a plan file. The variables used when\n" +
						"the plan was created will be used. If you wish to use different\n" +
						"variable values, create a new plan file.")
			}

			return plan.Context(opts), true, nil
		}
	}

	// Load up the state
	var state *terraform.State
	if copts.StatePath != "" {
		f, err := os.Open(copts.StatePath)
		if err != nil && os.IsNotExist(err) {
			// If the state file doesn't exist, it is okay, since it
			// is probably a new infrastructure.
			err = nil
		} else if err == nil {
			state, err = terraform.ReadState(f)
			f.Close()
		}

		if err != nil {
			return nil, false, fmt.Errorf("Error loading state: %s", err)
		}
	}

	// Store the loaded state
	m.state = state

	// Load the root module
	mod, err := module.NewTreeModule("", copts.Path)
	if err != nil {
		return nil, false, fmt.Errorf("Error loading config: %s", err)
	}
	err = mod.Load(m.moduleStorage(copts.Path), copts.GetMode)
	if err != nil {
		return nil, false, fmt.Errorf("Error downloading modules: %s", err)
	}

	opts.Module = mod
	opts.State = state
	ctx := terraform.NewContext(opts)
	return ctx, false, nil
}

// Input returns true if we should ask for input for context.
func (m *Meta) Input() bool {
	return !test && m.input && len(m.variables) == 0
}

// contextOpts returns the options to use to initialize a Terraform
// context with the settings from this Meta.
func (m *Meta) contextOpts() *terraform.ContextOpts {
	var opts terraform.ContextOpts = *m.ContextOpts
	opts.Hooks = make(
		[]terraform.Hook,
		len(m.ContextOpts.Hooks)+len(m.extraHooks)+1)
	opts.Hooks[0] = m.uiHook()
	copy(opts.Hooks[1:], m.ContextOpts.Hooks)
	copy(opts.Hooks[len(m.ContextOpts.Hooks)+1:], m.extraHooks)

	vs := make(map[string]string)
	for k, v := range opts.Variables {
		vs[k] = v
	}
	for k, v := range m.autoVariables {
		vs[k] = v
	}
	for k, v := range m.variables {
		vs[k] = v
	}
	opts.Variables = vs
	opts.UIInput = &UIInput{
		Colorize: m.Colorize(),
	}

	return &opts
}

// flags adds the meta flags to the given FlagSet.
func (m *Meta) flagSet(n string) *flag.FlagSet {
	f := flag.NewFlagSet(n, flag.ContinueOnError)
	f.BoolVar(&m.input, "input", true, "input")
	f.Var((*FlagVar)(&m.variables), "var", "variables")
	f.Var((*FlagVarFile)(&m.variables), "var-file", "variable file")

	if m.autoKey != "" {
		f.Var((*FlagVarFile)(&m.autoVariables), m.autoKey, "variable file")
	}

	// Create an io.Writer that writes to our Ui properly for errors.
	// This is kind of a hack, but it does the job. Basically: create
	// a pipe, use a scanner to break it into lines, and output each line
	// to the UI. Do this forever.
	errR, errW := io.Pipe()
	errScanner := bufio.NewScanner(errR)
	go func() {
		for errScanner.Scan() {
			m.Ui.Error(errScanner.Text())
		}
	}()
	f.SetOutput(errW)

	return f
}

// moduleStorage returns the module.Storage implementation used to store
// modules for commands.
func (m *Meta) moduleStorage(root string) module.Storage {
	return &uiModuleStorage{
		Storage: &module.FolderStorage{
			StorageDir: filepath.Join(root, "modules"),
		},
		Ui: m.Ui,
	}
}

// process will process the meta-parameters out of the arguments. This
// will potentially modify the args in-place. It will return the resulting
// slice.
func (m *Meta) process(args []string, vars bool) []string {
	// We do this so that we retain the ability to technically call
	// process multiple times, even if we have no plans to do so
	if m.oldUi != nil {
		m.Ui = m.oldUi
	}

	// Set colorization
	m.color = m.Color
	for i, v := range args {
		if v == "-no-color" {
			m.color = false
			args = append(args[:i], args[i+1:]...)
			break
		}
	}

	// Set the UI
	m.oldUi = m.Ui
	m.Ui = &cli.ConcurrentUi{
		Ui: &ColorizeUi{
			Colorize:   m.Colorize(),
			ErrorColor: "[red]",
			Ui:         m.oldUi,
		},
	}

	// If we support vars and the default var file exists, add it to
	// the args...
	m.autoKey = ""
	if vars {
		if _, err := os.Stat(DefaultVarsFilename); err == nil {
			m.autoKey = "var-file-default"
			args = append(args, "", "")
			copy(args[2:], args[0:])
			args[0] = "-" + m.autoKey
			args[1] = DefaultVarsFilename
		}
	}

	return args
}

// uiHook returns the UiHook to use with the context.
func (m *Meta) uiHook() *UiHook {
	return &UiHook{
		Colorize: m.Colorize(),
		Ui:       m.Ui,
	}
}

// contextOpts are the options used to load a context from a command.
type contextOpts struct {
	// Path to the directory where the root module is.
	Path string

	// StatePath is the path to the state file. If this is empty, then
	// no state will be loaded. It is also okay for this to be a path to
	// a file that doesn't exist; it is assumed that this means that there
	// is simply no state.
	StatePath string

	// GetMode is the module.GetMode to use when loading the module tree.
	GetMode module.GetMode
}
