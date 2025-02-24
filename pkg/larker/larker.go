package larker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/cirruslabs/cirrus-cli/pkg/larker/fs"
	"github.com/cirruslabs/cirrus-cli/pkg/larker/fs/cachinglayer"
	"github.com/cirruslabs/cirrus-cli/pkg/larker/fs/dummy"
	"github.com/cirruslabs/cirrus-cli/pkg/larker/loader"
	"github.com/cirruslabs/cirrus-cli/pkg/yamlhelper"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"gopkg.in/yaml.v3"
	"time"
)

var (
	ErrLoadFailed           = errors.New("load failed")
	ErrNotFound             = errors.New("entrypoint not found")
	ErrMainFailed           = errors.New("failed to call main")
	ErrHookFailed           = errors.New("failed to call hook")
	ErrMainUnexpectedResult = errors.New("main returned unexpected result")
	ErrSanity               = errors.New("sanity check failed")
)

type Larker struct {
	fs            fs.FileSystem
	env           map[string]string
	affectedFiles []string
	isTest        bool
}

type HookResult struct {
	ErrorMessage  string
	OutputLogs    []byte
	DurationNanos int64
	Result        interface{}
}

type MainResult struct {
	OutputLogs []byte
	YAMLConfig string
}

func New(opts ...Option) *Larker {
	lrk := &Larker{
		fs:  dummy.New(),
		env: make(map[string]string),
	}

	// weird global init by Starlark
	// we need floats at least for configuring CPUs for containers
	resolve.AllowFloat = true

	// Apply options
	for _, opt := range opts {
		opt(lrk)
	}

	// Wrap the final file system in a caching layer
	wrappedFS, err := cachinglayer.Wrap(lrk.fs)
	if err != nil {
		panic(err)
	}
	lrk.fs = wrappedFS

	return lrk
}

func (larker *Larker) MainOptional(ctx context.Context, source string) (*MainResult, error) {
	result, err := larker.Main(ctx, source)
	if errors.Is(err, ErrNotFound) {
		return &MainResult{
			OutputLogs: nil,
			YAMLConfig: "",
		}, nil
	}
	return result, err
}

func (larker *Larker) Main(ctx context.Context, source string) (*MainResult, error) {
	outputLogsBuffer := &bytes.Buffer{}
	capture := func(thread *starlark.Thread, msg string) {
		_, _ = fmt.Fprintln(outputLogsBuffer, msg)
	}

	thread := &starlark.Thread{
		Load:  loader.NewLoader(ctx, larker.fs, larker.env, larker.affectedFiles, larker.isTest).LoadFunc(larker.fs),
		Print: capture,
	}

	resCh := make(chan starlark.Value)
	errCh := make(chan error)

	go func() {
		// Execute the source code for the main() to be visible
		globals, err := starlark.ExecFile(thread, ".cirrus.star", source, nil)
		if err != nil {
			errCh <- fmt.Errorf("%w: %v", ErrLoadFailed, err)
			return
		}

		// Retrieve main()
		main, ok := globals["main"]
		if !ok {
			errCh <- fmt.Errorf("%w: main()", ErrNotFound)
			return
		}

		// Ensure that main() is a function
		mainFunc, ok := main.(*starlark.Function)
		if !ok {
			errCh <- fmt.Errorf("%w: main is not a function", ErrMainFailed)
			return
		}

		var args starlark.Tuple

		// Prepare a context to pass to main() as it's first argument if needed
		if mainFunc.NumParams() != 0 {
			args = append(args, &Context{})
		}

		mainResult, err := starlark.Call(thread, main, args, nil)
		if err != nil {
			errCh <- &ErrExecFailed{err: err}
			return
		}

		resCh <- mainResult
	}()

	var mainResult starlark.Value

	select {
	case mainResult = <-resCh:
	case err := <-errCh:
		return nil, &ExtendedError{err: err, logs: logsWithErrorAttached(outputLogsBuffer.Bytes(), err)}
	case <-ctx.Done():
		thread.Cancel(ctx.Err().Error())
		return nil, ctx.Err()
	}

	var tasksNode *yaml.Node
	var err error

	// main() should return a list of tasks or a dict resembling a Cirrus YAML configuration
	switch typedMainResult := mainResult.(type) {
	case *starlark.List:
		tasksNode, err = convertInstructions(typedMainResult)
		if err != nil {
			return nil, err
		}
		if tasksNode == nil {
			return &MainResult{OutputLogs: outputLogsBuffer.Bytes()}, nil
		}
	case *starlark.Dict:
		tasksNode = convertDict(typedMainResult)
		if tasksNode == nil {
			return &MainResult{OutputLogs: outputLogsBuffer.Bytes()}, nil
		}
	default:
		return nil, fmt.Errorf("%w: result is not a list or a dict", ErrMainUnexpectedResult)
	}

	formattedYaml, err := yamlhelper.PrettyPrint(tasksNode)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot marshal into YAML: %v", ErrMainUnexpectedResult, err)
	}

	return &MainResult{
		OutputLogs: outputLogsBuffer.Bytes(),
		YAMLConfig: formattedYaml,
	}, nil
}

func (larker *Larker) Hook(
	ctx context.Context,
	source string,
	name string,
	arguments []interface{},
) (*HookResult, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: empty hook name specified", ErrSanity)
	}

	outputLogsBuffer := &bytes.Buffer{}
	capture := func(thread *starlark.Thread, msg string) {
		_, _ = fmt.Fprintln(outputLogsBuffer, msg)
	}

	thread := &starlark.Thread{
		Load:  loader.NewLoader(ctx, larker.fs, larker.env, []string{}, larker.isTest).LoadFunc(larker.fs),
		Print: capture,
	}

	resCh := make(chan *HookResult)
	errCh := make(chan error)

	go func() {
		// Execute the source code for the hook to be visible
		globals, err := starlark.ExecFile(thread, ".cirrus.star", source, nil)
		if err != nil {
			errCh <- fmt.Errorf("%w: %v", ErrLoadFailed, err)
			return
		}

		// Retrieve hook
		hook, ok := globals[name]
		if !ok {
			errCh <- fmt.Errorf("%w: %s()", ErrNotFound, name)
			return
		}

		// Ensure that hook is a function
		hookFunc, ok := hook.(*starlark.Function)
		if !ok {
			errCh <- fmt.Errorf("%w: %s is not a function", ErrHookFailed, name)
			return
		}

		var args starlark.Tuple

		if hookFunc.NumParams() != 0 {
			for i, argument := range arguments {
				argumentStarlark, err := interfaceAsStarlarkValue(argument)
				if err != nil {
					errCh <- fmt.Errorf("%w: %s()'s %d argument should be JSON-compatible: %v",
						ErrHookFailed, name, i+1, err)
					return
				}

				args = append(args, argumentStarlark)
			}
		}

		// Run hook and measure time spent
		//
		// We could've used unix.Getrusage() here instead, however:
		// * it's not clear if we even need such level of precision at the moment
		// * precise time measurement requires:
		//   * usage of the Linux-specific RUSAGE_THREAD flag
		//   * guarding starlark.Call() with runtime.LockOSThread()/runtime.UnlockOSThread()
		hookStartTime := time.Now()

		hookResult, err := starlark.Call(thread, hook, args, nil)
		if err != nil {
			errCh <- &ErrExecFailed{err: err}
			return
		}

		durationNanos := time.Since(hookStartTime).Nanoseconds()

		// Convert Starlark-style value to interface{}-style value
		hookResultStarlark, err := starlarkValueAsInterface(hookResult)
		if err != nil {
			errCh <- err
			return
		}

		// All good
		resCh <- &HookResult{
			OutputLogs:    outputLogsBuffer.Bytes(),
			DurationNanos: durationNanos,
			Result:        hookResultStarlark,
		}
	}()

	select {
	case hookResult := <-resCh:
		return hookResult, nil
	case err := <-errCh:
		return &HookResult{
			ErrorMessage: err.Error(),
			OutputLogs:   logsWithErrorAttached(outputLogsBuffer.Bytes(), err),
		}, nil
	case <-ctx.Done():
		thread.Cancel(ctx.Err().Error())
		return nil, ctx.Err()
	}
}

func logsWithErrorAttached(logs []byte, err error) []byte {
	fmt.Printf("%T\n", err)

	ee, ok := errors.Unwrap(err).(*starlark.EvalError)
	if !ok {
		return logs
	}

	if len(logs) != 0 && !bytes.HasSuffix(logs, []byte("\n")) {
		logs = append(logs, byte('\n'))
	}

	logs = append(logs, []byte(ee.Backtrace())...)

	return logs
}
