// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package instrument

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dave/dst"

	"github.com/open-telemetry/opentelemetry-go-compile-instrumentation/tool/internal/ast"
	"github.com/open-telemetry/opentelemetry-go-compile-instrumentation/tool/internal/instrument/importcfg"
	"github.com/open-telemetry/opentelemetry-go-compile-instrumentation/tool/util"
)

type InstrumentPhase struct {
	logger *slog.Logger
	// The context for this phase
	ctx context.Context
	// The working directory during compilation
	workDir string
	// The importcfg configuration
	importConfig importcfg.ImportConfig
	// The path to the importcfg file
	importConfigPath string
	// The target file to be instrumented
	target *dst.File
	// The parser for the target file
	parser *ast.AstParser
	// The compiling arguments for the target file
	compileArgs []string
	// The target function to be instrumented
	targetFunc *dst.FuncDecl
	// The before trampoline function
	beforeTrampFunc *dst.FuncDecl
	// The after trampoline function
	afterTrampFunc *dst.FuncDecl
	// Variable declarations waiting to be inserted into target source file
	varDecls []dst.Decl
	// The declaration of the hook context, it should be populated later
	hookCtxDecl *dst.GenDecl
	// The methods of the hook context
	hookCtxMethods []*dst.FuncDecl
	// The trampoline jumps to be optimized
	tjumps []*TJump
}

func (ip *InstrumentPhase) Info(msg string, args ...any)  { ip.logger.Info(msg, args...) }
func (ip *InstrumentPhase) Error(msg string, args ...any) { ip.logger.Error(msg, args...) }
func (ip *InstrumentPhase) Warn(msg string, args ...any)  { ip.logger.Warn(msg, args...) }
func (ip *InstrumentPhase) Debug(msg string, args ...any) { ip.logger.Debug(msg, args...) }

// keepForDebug keeps the the file to .otel-build directory for debugging
func (ip *InstrumentPhase) keepForDebug(name string) {
	escape := func(s string) string {
		dirName := strings.ReplaceAll(s, "/", "_")
		dirName = strings.ReplaceAll(dirName, ".", "_")
		return dirName
	}
	modPath := util.FindFlagValue(ip.compileArgs, "-p")
	dest := filepath.Join("debug", escape(modPath), filepath.Base(name))
	err := util.CopyFile(name, util.GetBuildTemp(dest))
	if err != nil { // error is tolerable here as this is only for debugging
		ip.Warn("failed to save modified file", "dest", dest, "error", err)
	}
}

func stripCompleteFlag(args []string) []string {
	for i, arg := range args {
		if arg == "-complete" {
			return append(args[:i], args[i+1:]...)
		}
	}
	return args
}

func interceptCompile(ctx context.Context, args []string) ([]string, error) {
	// Read compilation output directory
	target := util.FindFlagValue(args, "-o")
	util.Assert(target != "", "missing -o flag value")

	// Extract -importcfg flag
	importCfgPath := util.FindFlagValue(args, "-importcfg")

	ip := &InstrumentPhase{
		logger:           util.LoggerFromContext(ctx),
		ctx:              ctx,
		workDir:          filepath.Dir(target),
		compileArgs:      args,
		importConfigPath: importCfgPath,
	}

	// Parse existing importcfg if present
	if importCfgPath != "" {
		imports, err := importcfg.ParseFile(importCfgPath)
		if err != nil {
			return nil, fmt.Errorf("parsing importcfg: %w", err)
		}
		ip.importConfig = imports
	}

	// Load matched hook rules from setup phase
	allSet, err := ip.load()
	if err != nil {
		return nil, err
	}

	// Check if the current compile command matches the rules.
	matched := ip.match(allSet, args)
	if !matched.IsEmpty() {
		ip.Info("Instrument package", "rules", matched, "args", args)
		// Okay, this package should be instrumented.
		err = ip.instrument(matched)
		if err != nil {
			return nil, err
		}

		// Strip -complete flag as we may insert some hook points that are
		// not ready yet, i.e. they don't have function body
		ip.compileArgs = stripCompleteFlag(ip.compileArgs)
		ip.Info("Run instrumented command", "args", ip.compileArgs)
	}

	return ip.compileArgs, nil
}

// updateImportConfig updates the importcfg file with new imports that were added during instrumentation.
func (ip *InstrumentPhase) updateImportConfig(newImports map[string]string) error {
	if ip.importConfigPath == "" {
		// No importcfg file, skip (shouldn't happen in normal builds)
		return nil
	}

	var updated bool
	for _, importPath := range newImports {
		if importPath == "unsafe" {
			// unsafe is built-in, no archive file needed
			continue
		}

		if _, exists := ip.importConfig.PackageFile[importPath]; exists {
			// Already have this import
			continue
		}

		// Resolve package archive location
		archives, err := importcfg.ResolvePackageFiles(ip.ctx, importPath)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", importPath, err)
		}

		for pkg, archive := range archives {
			if _, exists := ip.importConfig.PackageFile[pkg]; !exists {
				ip.Debug("Adding import to importcfg", "package", pkg, "archive", archive)
				ip.importConfig.PackageFile[pkg] = archive
				updated = true
			}
		}
	}

	if !updated {
		return nil
	}

	// Backup original
	backupPath := ip.importConfigPath + ".original"
	if err := os.Rename(ip.importConfigPath, backupPath); err != nil {
		return fmt.Errorf("backing up importcfg: %w", err)
	}

	// Write updated importcfg
	if err := ip.importConfig.WriteFile(ip.importConfigPath); err != nil {
		return fmt.Errorf("writing updated importcfg: %w", err)
	}

	ip.Info("Updated importcfg", "path", ip.importConfigPath)
	return nil
}

// Toolexec is the entry point of the toolexec command. It intercepts all the
// commands(link, compile, asm, etc) during build process. Our responsibility is
// to find out the compile command we are interested in and run it with the
// instrumented code.
func Toolexec(ctx context.Context, args []string) error {
	// Only interested in compile commands
	if util.IsCompileCommand(strings.Join(args, " ")) {
		var err error
		args, err = interceptCompile(ctx, args)
		if err != nil {
			return err
		}
	}
	// Just run the command as is
	return util.RunCmd(ctx, args...)
}
