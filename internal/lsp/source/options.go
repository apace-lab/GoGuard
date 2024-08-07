// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/bozhen-liu/gopa/go/analysis"
	"github.com/bozhen-liu/gopa/go/analysis/passes/asmdecl"
	"github.com/bozhen-liu/gopa/go/analysis/passes/assign"
	"github.com/bozhen-liu/gopa/go/analysis/passes/atomic"
	"github.com/bozhen-liu/gopa/go/analysis/passes/atomicalign"
	"github.com/bozhen-liu/gopa/go/analysis/passes/bools"
	"github.com/bozhen-liu/gopa/go/analysis/passes/buildtag"
	"github.com/bozhen-liu/gopa/go/analysis/passes/cgocall"
	"github.com/bozhen-liu/gopa/go/analysis/passes/composite"
	"github.com/bozhen-liu/gopa/go/analysis/passes/copylock"
	"github.com/bozhen-liu/gopa/go/analysis/passes/deepequalerrors"
	"github.com/bozhen-liu/gopa/go/analysis/passes/errorsas"
	"github.com/bozhen-liu/gopa/go/analysis/passes/httpresponse"
	"github.com/bozhen-liu/gopa/go/analysis/passes/loopclosure"
	"github.com/bozhen-liu/gopa/go/analysis/passes/lostcancel"
	"github.com/bozhen-liu/gopa/go/analysis/passes/nilfunc"
	"github.com/bozhen-liu/gopa/go/analysis/passes/printf"
	"github.com/bozhen-liu/gopa/go/analysis/passes/shift"
	"github.com/bozhen-liu/gopa/go/analysis/passes/sortslice"
	"github.com/bozhen-liu/gopa/go/analysis/passes/stdmethods"
	"github.com/bozhen-liu/gopa/go/analysis/passes/structtag"
	"github.com/bozhen-liu/gopa/go/analysis/passes/testinggoroutine"
	"github.com/bozhen-liu/gopa/go/analysis/passes/tests"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unmarshal"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unreachable"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unsafeptr"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unusedresult"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/fillreturns"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/fillstruct"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/nonewvars"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/noresultvalues"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/simplifycompositelit"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/simplifyrange"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/simplifyslice"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/undeclaredname"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/unusedparams"
	"github.com/bozhen-liu/gopa/internal/lsp/diff"
	"github.com/bozhen-liu/gopa/internal/lsp/diff/myers"
	"github.com/bozhen-liu/gopa/internal/lsp/protocol"
	errors "golang.org/x/xerrors"
)

// DefaultOptions is the options that are used for Gopls execution independent
// of any externally provided configuration (LSP initialization, command
// invokation, etc.).
func DefaultOptions() Options {
	var commands []string
	for _, c := range Commands {
		commands = append(commands, c.Name)
	}
	return Options{
		ClientOptions: ClientOptions{
			InsertTextFormat:                  protocol.PlainTextTextFormat,
			PreferredContentFormat:            protocol.Markdown,
			ConfigurationSupported:            true,
			DynamicConfigurationSupported:     true,
			DynamicWatchedFilesSupported:      true,
			LineFoldingOnly:                   false,
			HierarchicalDocumentSymbolSupport: true,
		},
		ServerOptions: ServerOptions{
			SupportedCodeActions: map[FileKind]map[protocol.CodeActionKind]bool{
				Go: {
					protocol.SourceFixAll:          true,
					protocol.SourceOrganizeImports: true,
					protocol.QuickFix:              true,
					protocol.RefactorRewrite:       true,
					protocol.RefactorExtract:       true,
				},
				Mod: {
					protocol.SourceOrganizeImports: true,
				},
				Sum: {},
			},
			SupportedCommands: commands,
		},
		UserOptions: UserOptions{
			Env:                     os.Environ(),
			HoverKind:               FullDocumentation,
			LinkTarget:              "pkg.go.dev",
			LinksInHover:            true,
			Matcher:                 Fuzzy,
			SymbolMatcher:           SymbolFuzzy,
			DeepCompletion:          true,
			UnimportedCompletion:    true,
			CompletionDocumentation: true,
			EnabledCodeLens: map[string]bool{
				CommandGenerate.Name:          true,
				CommandRegenerateCgo.Name:     true,
				CommandTidy.Name:              true,
				CommandToggleDetails.Name:     false,
				CommandUpgradeDependency.Name: true,
				CommandVendor.Name:            true,
			},
			ExpandWorkspaceToModule: true,
		},
		DebuggingOptions: DebuggingOptions{
			CompletionBudget:   100 * time.Millisecond,
			LiteralCompletions: true,
		},
		ExperimentalOptions: ExperimentalOptions{
			TempModfile: true,
		},
		Hooks: Hooks{
			ComputeEdits:         myers.ComputeEdits,
			URLRegexp:            urlRegexp(),
			DefaultAnalyzers:     defaultAnalyzers(),
			TypeErrorAnalyzers:   typeErrorAnalyzers(),
			ConvenienceAnalyzers: convenienceAnalyzers(),
			GoDiff:               true,
		},
	}
}

// Options holds various configuration that affects Gopls execution, organized
// by the nature or origin of the settings.
type Options struct {
	ClientOptions
	ServerOptions
	UserOptions
	DebuggingOptions
	ExperimentalOptions
	Hooks
}

// ClientOptions holds LSP-specific configuration that is provided by the
// client.
type ClientOptions struct {
	InsertTextFormat                  protocol.InsertTextFormat
	ConfigurationSupported            bool
	DynamicConfigurationSupported     bool
	DynamicWatchedFilesSupported      bool
	PreferredContentFormat            protocol.MarkupKind
	LineFoldingOnly                   bool
	HierarchicalDocumentSymbolSupport bool
}

// ServerOptions holds LSP-specific configuration that is provided by the
// server.
type ServerOptions struct {
	SupportedCodeActions map[FileKind]map[protocol.CodeActionKind]bool
	SupportedCommands    []string
}

// UserOptions holds custom Gopls configuration (not part of the LSP) that is
// modified by the client.
type UserOptions struct {
	// Env is the current set of environment overrides on this view.
	Env []string

	// BuildFlags is used to adjust the build flags applied to the view.
	BuildFlags []string

	// HoverKind specifies the format of the content for hover requests.
	HoverKind HoverKind

	// UserEnabledAnalyses specifies analyses that the user would like to enable
	// or disable. A map of the names of analysis passes that should be
	// enabled/disabled. A full list of analyzers that gopls uses can be found
	// [here](analyzers.md).
	//
	// Example Usage:
	// ...
	// "analyses": {
	//   "unreachable": false, // Disable the unreachable analyzer.
	//   "unusedparams": true  // Enable the unusedparams analyzer.
	// }
	UserEnabledAnalyses map[string]bool

	// EnabledCodeLens specifies which codelens are enabled, keyed by the gopls
	// command that they provide.
	EnabledCodeLens map[string]bool

	// StaticCheck enables additional analyses from staticcheck.io.
	StaticCheck bool

	// LinkTarget is the website used for documentation. If empty, no link is
	// provided.
	LinkTarget string

	// LinksInHover toggles the presence of links to documentation in hover.
	LinksInHover bool

	// ImportShortcut specifies whether import statements should link to
	// documentation or go to definitions. The default is both.
	ImportShortcut ImportShortcut

	// LocalPrefix is used to specify goimports's -local behavior.
	LocalPrefix string

	// Matcher specifies the type of matcher to use for completion requests.
	Matcher Matcher

	// SymbolMatcher specifies the type of matcher to use for symbol requests.
	SymbolMatcher SymbolMatcher

	// SymbolStyle specifies what style of symbols to return in symbol requests
	// (package qualified, fully qualified, etc).
	SymbolStyle SymbolStyle

	// DeepCompletion allows completion to perform nested searches through
	// possible candidates.
	DeepCompletion bool

	// UnimportedCompletion enables completion for unimported packages.
	UnimportedCompletion bool

	// CompletionDocumentation returns additional documentation with completion
	// requests.
	CompletionDocumentation bool

	// Placeholders adds placeholders to parameters and structs in completion
	// results.
	Placeholders bool

	// Gofumpt indicates if we should run gofumpt formatting.
	Gofumpt bool

	// ExpandWorkspaceToModule is true if we should expand the scope of the
	// workspace to include the modules containing the workspace folders.
	ExpandWorkspaceToModule bool
}

type ImportShortcut int

const (
	Both ImportShortcut = iota
	Link
	Definition
)

func (s ImportShortcut) ShowLinks() bool {
	return s == Both || s == Link
}

func (s ImportShortcut) ShowDefinition() bool {
	return s == Both || s == Definition
}

type completionOptions struct {
	deepCompletion    bool
	unimported        bool
	documentation     bool
	fullDocumentation bool
	placeholders      bool
	literal           bool
	matcher           Matcher
	budget            time.Duration
}

// Hooks contains configuration that is provided to the Gopls command by the
// main package.
type Hooks struct {
	GoDiff               bool
	ComputeEdits         diff.ComputeEdits
	URLRegexp            *regexp.Regexp
	DefaultAnalyzers     map[string]Analyzer
	TypeErrorAnalyzers   map[string]Analyzer
	ConvenienceAnalyzers map[string]Analyzer
	GofumptFormat        func(ctx context.Context, src []byte) ([]byte, error)
}

func (o Options) AddDefaultAnalyzer(a *analysis.Analyzer) {
	o.DefaultAnalyzers[a.Name] = Analyzer{Analyzer: a, enabled: true}
}

// ExperimentalOptions defines configuration for features under active
// development. WARNING: This configuration will be changed in the future. It
// only exists while these features are under development.
type ExperimentalOptions struct {
	// TempModfile controls the use of the -modfile flag in Go 1.14.
	TempModfile bool

	// VerboseWorkDoneProgress controls whether the LSP server should send
	// progress reports for all work done outside the scope of an RPC.
	VerboseWorkDoneProgress bool

	// Annotations suppress various kinds of optimization diagnostics
	// that would be reported by the gc_details command.
	//   noNilcheck suppresses display of nilchecks.
	//   noEscape suppresses escape choices.
	//   noInline suppresses inlining choices.
	//   noBounds suppresses bounds checking diagnositcs.
	Annotations map[string]bool
}

// DebuggingOptions should not affect the logical execution of Gopls, but may
// be altered for debugging purposes.
type DebuggingOptions struct {
	VerboseOutput bool

	// CompletionBudget is the soft latency goal for completion requests. Most
	// requests finish in a couple milliseconds, but in some cases deep
	// completions can take much longer. As we use up our budget we
	// dynamically reduce the search scope to ensure we return timely
	// results. Zero means unlimited.
	CompletionBudget time.Duration

	// LiteralCompletions controls whether literal candidates such as
	// "&someStruct{}" are offered. Tests disable this flag to simplify
	// their expected values.
	LiteralCompletions bool
}

type Matcher int

const (
	Fuzzy = Matcher(iota)
	CaseInsensitive
	CaseSensitive
)

type SymbolMatcher int

const (
	SymbolFuzzy = SymbolMatcher(iota)
	SymbolCaseInsensitive
	SymbolCaseSensitive
)

type SymbolStyle int

const (
	PackageQualifiedSymbols = SymbolStyle(iota)
	FullyQualifiedSymbols
	DynamicSymbols
)

type HoverKind int

const (
	SingleLine = HoverKind(iota)
	NoDocumentation
	SynopsisDocumentation
	FullDocumentation

	// Structured is an experimental setting that returns a structured hover format.
	// This format separates the signature from the documentation, so that the client
	// can do more manipulation of these fields.
	//
	// This should only be used by clients that support this behavior.
	Structured
)

type OptionResults []OptionResult

type OptionResult struct {
	Name  string
	Value interface{}
	Error error

	State       OptionState
	Replacement string
}

type OptionState int

const (
	OptionHandled = OptionState(iota)
	OptionDeprecated
	OptionUnexpected
)

type LinkTarget string

func SetOptions(options *Options, opts interface{}) OptionResults {
	var results OptionResults
	switch opts := opts.(type) {
	case nil:
	case map[string]interface{}:
		for name, value := range opts {
			results = append(results, options.set(name, value))
		}
	default:
		results = append(results, OptionResult{
			Value: opts,
			Error: errors.Errorf("Invalid options type %T", opts),
		})
	}
	return results
}

func (o *Options) ForClientCapabilities(caps protocol.ClientCapabilities) {
	// Check if the client supports snippets in completion items.
	if c := caps.TextDocument.Completion; c.CompletionItem.SnippetSupport {
		o.InsertTextFormat = protocol.SnippetTextFormat
	}
	// Check if the client supports configuration messages.
	o.ConfigurationSupported = caps.Workspace.Configuration
	o.DynamicConfigurationSupported = caps.Workspace.DidChangeConfiguration.DynamicRegistration
	o.DynamicWatchedFilesSupported = caps.Workspace.DidChangeWatchedFiles.DynamicRegistration

	// Check which types of content format are supported by this client.
	if hover := caps.TextDocument.Hover; len(hover.ContentFormat) > 0 {
		o.PreferredContentFormat = hover.ContentFormat[0]
	}
	// Check if the client supports only line folding.
	fr := caps.TextDocument.FoldingRange
	o.LineFoldingOnly = fr.LineFoldingOnly
	// Check if the client supports hierarchical document symbols.
	o.HierarchicalDocumentSymbolSupport = caps.TextDocument.DocumentSymbol.HierarchicalDocumentSymbolSupport
}

func (o *Options) set(name string, value interface{}) OptionResult {
	result := OptionResult{Name: name, Value: value}
	switch name {
	case "env":
		menv, ok := value.(map[string]interface{})
		if !ok {
			result.errorf("invalid config gopls.env type %T", value)
			break
		}
		for k, v := range menv {
			o.Env = append(o.Env, fmt.Sprintf("%s=%s", k, v))
		}

	case "buildFlags":
		iflags, ok := value.([]interface{})
		if !ok {
			result.errorf("invalid config gopls.buildFlags type %T", value)
			break
		}
		flags := make([]string, 0, len(iflags))
		for _, flag := range iflags {
			flags = append(flags, fmt.Sprintf("%s", flag))
		}
		o.BuildFlags = flags

	case "completionDocumentation":
		result.setBool(&o.CompletionDocumentation)
	case "usePlaceholders":
		result.setBool(&o.Placeholders)
	case "deepCompletion":
		result.setBool(&o.DeepCompletion)
	case "completeUnimported":
		result.setBool(&o.UnimportedCompletion)
	case "completionBudget":
		if v, ok := result.asString(); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				result.errorf("failed to parse duration %q: %v", v, err)
				break
			}
			o.CompletionBudget = d
		}

	case "matcher":
		matcher, ok := result.asString()
		if !ok {
			break
		}
		switch matcher {
		case "fuzzy":
			o.Matcher = Fuzzy
		case "caseSensitive":
			o.Matcher = CaseSensitive
		default:
			o.Matcher = CaseInsensitive
		}

	case "symbolMatcher":
		matcher, ok := result.asString()
		if !ok {
			break
		}
		switch matcher {
		case "fuzzy":
			o.SymbolMatcher = SymbolFuzzy
		case "caseSensitive":
			o.SymbolMatcher = SymbolCaseSensitive
		default:
			o.SymbolMatcher = SymbolCaseInsensitive
		}

	case "symbolStyle":
		style, ok := result.asString()
		if !ok {
			break
		}
		switch style {
		case "full":
			o.SymbolStyle = FullyQualifiedSymbols
		case "dynamic":
			o.SymbolStyle = DynamicSymbols
		case "package":
			o.SymbolStyle = PackageQualifiedSymbols
		default:
			result.errorf("Unsupported symbol style %q", style)
		}

	case "hoverKind":
		hoverKind, ok := result.asString()
		if !ok {
			break
		}
		switch hoverKind {
		case "NoDocumentation":
			o.HoverKind = NoDocumentation
		case "SingleLine":
			o.HoverKind = SingleLine
		case "SynopsisDocumentation":
			o.HoverKind = SynopsisDocumentation
		case "FullDocumentation":
			o.HoverKind = FullDocumentation
		case "Structured":
			o.HoverKind = Structured
		default:
			result.errorf("Unsupported hover kind %q", hoverKind)
		}

	case "linkTarget":
		result.setString(&o.LinkTarget)

	case "linksInHover":
		result.setBool(&o.LinksInHover)

	case "importShortcut":
		var s string
		result.setString(&s)
		switch s {
		case "both":
			o.ImportShortcut = Both
		case "link":
			o.ImportShortcut = Link
		case "definition":
			o.ImportShortcut = Definition
		}

	case "analyses":
		result.setBoolMap(&o.UserEnabledAnalyses)

	case "annotations":
		result.setBoolMap(&o.Annotations)
		for k := range o.Annotations {
			switch k {
			case "noEscape", "noNilcheck", "noInline", "noBounds":
				continue
			default:
				result.Name += ":" + k // put mistake(s) in the message
				result.State = OptionUnexpected
			}
		}

	case "codelens":
		var lensOverrides map[string]bool
		result.setBoolMap(&lensOverrides)
		if result.Error == nil {
			if o.EnabledCodeLens == nil {
				o.EnabledCodeLens = make(map[string]bool)
			}
			for lens, enabled := range lensOverrides {
				o.EnabledCodeLens[lens] = enabled
			}
		}

	case "staticcheck":
		result.setBool(&o.StaticCheck)

	case "local":
		result.setString(&o.LocalPrefix)

	case "verboseOutput":
		result.setBool(&o.VerboseOutput)

	case "verboseWorkDoneProgress":
		result.setBool(&o.VerboseWorkDoneProgress)

	case "tempModfile":
		result.setBool(&o.TempModfile)

	case "gofumpt":
		result.setBool(&o.Gofumpt)

	case "expandWorkspaceToModule":
		result.setBool(&o.ExpandWorkspaceToModule)

	// Replaced settings.
	case "experimentalDisabledAnalyses":
		result.State = OptionDeprecated
		result.Replacement = "analyses"

	case "disableDeepCompletion":
		result.State = OptionDeprecated
		result.Replacement = "deepCompletion"

	case "disableFuzzyMatching":
		result.State = OptionDeprecated
		result.Replacement = "fuzzyMatching"

	case "wantCompletionDocumentation":
		result.State = OptionDeprecated
		result.Replacement = "completionDocumentation"

	case "wantUnimportedCompletions":
		result.State = OptionDeprecated
		result.Replacement = "completeUnimported"

	case "fuzzyMatching":
		result.State = OptionDeprecated
		result.Replacement = "matcher"

	case "caseSensitiveCompletion":
		result.State = OptionDeprecated
		result.Replacement = "matcher"

	// Deprecated settings.
	case "wantSuggestedFixes":
		result.State = OptionDeprecated

	case "noIncrementalSync":
		result.State = OptionDeprecated

	case "watchFileChanges":
		result.State = OptionDeprecated

	case "go-diff":
		result.State = OptionDeprecated

	default:
		result.State = OptionUnexpected
	}
	return result
}

func (r *OptionResult) errorf(msg string, values ...interface{}) {
	r.Error = errors.Errorf(msg, values...)
}

func (r *OptionResult) asBool() (bool, bool) {
	b, ok := r.Value.(bool)
	if !ok {
		r.errorf("Invalid type %T for bool option %q", r.Value, r.Name)
		return false, false
	}
	return b, true
}

func (r *OptionResult) setBool(b *bool) {
	if v, ok := r.asBool(); ok {
		*b = v
	}
}

func (r *OptionResult) setBoolMap(bm *map[string]bool) {
	all, ok := r.Value.(map[string]interface{})
	if !ok {
		r.errorf("Invalid type %T for map[string]interface{} option %q", r.Value, r.Name)
		return
	}
	m := make(map[string]bool)
	for a, enabled := range all {
		if enabled, ok := enabled.(bool); ok {
			m[a] = enabled
		} else {
			r.errorf("Invalid type %d for map key %q in option %q", a, r.Name)
			return
		}
	}
	*bm = m
}

func (r *OptionResult) asString() (string, bool) {
	b, ok := r.Value.(string)
	if !ok {
		r.errorf("Invalid type %T for string option %q", r.Value, r.Name)
		return "", false
	}
	return b, true
}

func (r *OptionResult) setString(s *string) {
	if v, ok := r.asString(); ok {
		*s = v
	}
}

// EnabledAnalyzers returns all of the analyzers enabled for the given
// snapshot.
func EnabledAnalyzers(snapshot Snapshot) (analyzers []Analyzer) {
	for _, a := range snapshot.View().Options().DefaultAnalyzers {
		if a.Enabled(snapshot.View()) {
			analyzers = append(analyzers, a)
		}
	}
	for _, a := range snapshot.View().Options().TypeErrorAnalyzers {
		if a.Enabled(snapshot.View()) {
			analyzers = append(analyzers, a)
		}
	}
	for _, a := range snapshot.View().Options().ConvenienceAnalyzers {
		if a.Enabled(snapshot.View()) {
			analyzers = append(analyzers, a)
		}
	}
	return analyzers
}

func typeErrorAnalyzers() map[string]Analyzer {
	return map[string]Analyzer{
		fillreturns.Analyzer.Name: {
			Analyzer:       fillreturns.Analyzer,
			FixesError:     fillreturns.FixesError,
			HighConfidence: true,
			enabled:        true,
		},
		nonewvars.Analyzer.Name: {
			Analyzer:   nonewvars.Analyzer,
			FixesError: nonewvars.FixesError,
			enabled:    true,
		},
		noresultvalues.Analyzer.Name: {
			Analyzer:   noresultvalues.Analyzer,
			FixesError: noresultvalues.FixesError,
			enabled:    true,
		},
		undeclaredname.Analyzer.Name: {
			Analyzer:   undeclaredname.Analyzer,
			FixesError: undeclaredname.FixesError,
			Command:    CommandUndeclaredName,
			enabled:    true,
		},
	}
}

func convenienceAnalyzers() map[string]Analyzer {
	return map[string]Analyzer{
		fillstruct.Analyzer.Name: {
			Analyzer: fillstruct.Analyzer,
			Command:  CommandFillStruct,
			enabled:  true,
		},
	}
}

func defaultAnalyzers() map[string]Analyzer {
	return map[string]Analyzer{
		// The traditional vet suite:
		asmdecl.Analyzer.Name:      {Analyzer: asmdecl.Analyzer, enabled: true},
		assign.Analyzer.Name:       {Analyzer: assign.Analyzer, enabled: true},
		atomic.Analyzer.Name:       {Analyzer: atomic.Analyzer, enabled: true},
		atomicalign.Analyzer.Name:  {Analyzer: atomicalign.Analyzer, enabled: true},
		bools.Analyzer.Name:        {Analyzer: bools.Analyzer, enabled: true},
		buildtag.Analyzer.Name:     {Analyzer: buildtag.Analyzer, enabled: true},
		cgocall.Analyzer.Name:      {Analyzer: cgocall.Analyzer, enabled: true},
		composite.Analyzer.Name:    {Analyzer: composite.Analyzer, enabled: true},
		copylock.Analyzer.Name:     {Analyzer: copylock.Analyzer, enabled: true},
		errorsas.Analyzer.Name:     {Analyzer: errorsas.Analyzer, enabled: true},
		httpresponse.Analyzer.Name: {Analyzer: httpresponse.Analyzer, enabled: true},
		loopclosure.Analyzer.Name:  {Analyzer: loopclosure.Analyzer, enabled: true},
		lostcancel.Analyzer.Name:   {Analyzer: lostcancel.Analyzer, enabled: true},
		nilfunc.Analyzer.Name:      {Analyzer: nilfunc.Analyzer, enabled: true},
		printf.Analyzer.Name:       {Analyzer: printf.Analyzer, enabled: true},
		shift.Analyzer.Name:        {Analyzer: shift.Analyzer, enabled: true},
		stdmethods.Analyzer.Name:   {Analyzer: stdmethods.Analyzer, enabled: true},
		structtag.Analyzer.Name:    {Analyzer: structtag.Analyzer, enabled: true},
		tests.Analyzer.Name:        {Analyzer: tests.Analyzer, enabled: true},
		unmarshal.Analyzer.Name:    {Analyzer: unmarshal.Analyzer, enabled: true},
		unreachable.Analyzer.Name:  {Analyzer: unreachable.Analyzer, enabled: true},
		unsafeptr.Analyzer.Name:    {Analyzer: unsafeptr.Analyzer, enabled: true},
		unusedresult.Analyzer.Name: {Analyzer: unusedresult.Analyzer, enabled: true},

		// Non-vet analyzers:
		deepequalerrors.Analyzer.Name:  {Analyzer: deepequalerrors.Analyzer, enabled: true},
		sortslice.Analyzer.Name:        {Analyzer: sortslice.Analyzer, enabled: true},
		testinggoroutine.Analyzer.Name: {Analyzer: testinggoroutine.Analyzer, enabled: true},
		unusedparams.Analyzer.Name:     {Analyzer: unusedparams.Analyzer, enabled: false},

		// gofmt -s suite:
		simplifycompositelit.Analyzer.Name: {Analyzer: simplifycompositelit.Analyzer, enabled: true, HighConfidence: true},
		simplifyrange.Analyzer.Name:        {Analyzer: simplifyrange.Analyzer, enabled: true, HighConfidence: true},
		simplifyslice.Analyzer.Name:        {Analyzer: simplifyslice.Analyzer, enabled: true, HighConfidence: true},
	}
}

func urlRegexp() *regexp.Regexp {
	// Ensure links are matched as full words, not anywhere.
	re := regexp.MustCompile(`\b(http|ftp|https)://([\w_-]+(?:(?:\.[\w_-]+)+))([\w.,@?^=%&:/~+#-]*[\w@?^=%&/~+#-])?\b`)
	re.Longest()
	return re
}
