// Copyright 2020 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// It may or may not work on other OSes.
// If you test on another OS and it works, enable it.
//go:build linux

package cover

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/syzkaller/pkg/cover/backend"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/symbolizer"
	_ "github.com/google/syzkaller/sys"
	"github.com/google/syzkaller/sys/targets"
	"github.com/stretchr/testify/assert"
)

type Test struct {
	Name      string
	CFlags    []string
	LDFlags   []string
	Progs     []Prog
	DebugInfo bool
	AddCover  bool
	AddBadPc  bool
	// Set to true if the test should be skipped under broken kcov.
	SkipIfKcovIsBroken bool
	// Inexact coverage generated by AddCover=true may override empty Result.
	Result   string
	Supports func(target *targets.Target) bool
}

func TestReportGenerator(t *testing.T) {
	tests := []Test{
		{
			Name:      "no-coverage",
			DebugInfo: true,
			AddCover:  true,
			Result:    `.* doesn't contain coverage callbacks \(set CONFIG_KCOV=y on linux\)`,
		},
		{
			Name:     "no-debug-info",
			CFlags:   []string{"-fsanitize-coverage=trace-pc"},
			AddCover: true,
			Result:   `failed to parse DWARF.*\(set CONFIG_DEBUG_INFO=y on linux\)`,
		},
		{
			Name:      "no-pcs",
			CFlags:    []string{"-fsanitize-coverage=trace-pc"},
			DebugInfo: true,
			Result:    `no coverage collected so far`,
		},
		{
			Name:      "bad-pcs",
			CFlags:    []string{"-fsanitize-coverage=trace-pc"},
			DebugInfo: true,
			Progs:     []Prog{{Data: "data", PCs: []uint64{0x1, 0x2}}},
			Result:    `coverage doesn't match any coverage callbacks`,
		},
		{
			Name:      "good",
			AddCover:  true,
			CFlags:    []string{"-fsanitize-coverage=trace-pc"},
			DebugInfo: true,
		},
		{
			Name:               "mismatch-pcs",
			AddCover:           true,
			AddBadPc:           true,
			CFlags:             []string{"-fsanitize-coverage=trace-pc"},
			DebugInfo:          true,
			SkipIfKcovIsBroken: true,
			Result:             `.* do not have matching coverage callbacks`,
		},
		{
			Name:      "good-pie",
			AddCover:  true,
			CFlags:    []string{"-fsanitize-coverage=trace-pc", "-fpie"},
			LDFlags:   []string{"-pie", "-Wl,--section-start=.text=0x33300000"},
			DebugInfo: true,
			Supports: func(target *targets.Target) bool {
				return target.OS == targets.Fuchsia ||
					// Fails with "relocation truncated to fit: R_AARCH64_CALL26 against symbol `memcpy'".
					target.OS == targets.Linux && target.Arch != targets.ARM64
			},
		},
		{
			Name:     "good-pie-relocs",
			AddCover: true,
			// This produces a binary that resembles CONFIG_RANDOMIZE_BASE=y.
			// Symbols and .text section has addresses around 0x33300000,
			// but debug info has all PC ranges around 0 address.
			CFlags:    []string{"-fsanitize-coverage=trace-pc", "-fpie"},
			LDFlags:   []string{"-pie", "-Wl,--section-start=.text=0x33300000,--emit-relocs"},
			DebugInfo: true,
			Supports: func(target *targets.Target) bool {
				if target.OS == targets.Fuchsia {
					return true
				}
				if target.OS == targets.Linux {
					if target.Arch == targets.RiscV64 {
						// When the binary is compiled with gcc and parsed with
						// llvm-addr2line, we get an invalid "func_name", which
						// breaks our tests.
						fmt.Printf("target.CCompiler=%s", target.CCompiler)
						return target.CCompiler == "clang"
					}
					if target.Arch == targets.ARM64 || target.Arch == targets.ARM ||
						target.Arch == targets.I386 {
						return false
					}
					return true
				}
				return false
			},
		},
	}
	t.Parallel()
	for os, arches := range targets.List {
		if os == targets.TestOS {
			continue
		}
		for _, target := range arches {
			target := targets.Get(target.OS, target.Arch)
			if target.BuildOS != runtime.GOOS {
				continue
			}
			t.Run(target.OS+"-"+target.Arch, func(t *testing.T) {
				t.Parallel()
				if target.BrokenCompiler != "" {
					t.Skip("skipping the test due to broken cross-compiler:\n" + target.BrokenCompiler)
				}
				for _, test := range tests {
					t.Run(test.Name, func(t *testing.T) {
						if test.Supports != nil && !test.Supports(target) {
							t.Skip("unsupported target")
						}
						t.Parallel()
						testReportGenerator(t, target, test)
					})
				}
			})
		}
	}
}

func testReportGenerator(t *testing.T, target *targets.Target, test Test) {
	reps, err := generateReport(t, target, &test)
	if err != nil {
		if test.Result == "" {
			t.Fatalf("expected no error, but got:\n%v", err)
		}
		if !regexp.MustCompile(test.Result).MatchString(err.Error()) {
			t.Fatalf("expected error %q, but got:\n%v", test.Result, err)
		}
		return
	}
	if test.Result != "" {
		t.Fatalf("got no error, but expected %q", test.Result)
	}
	checkCSVReport(t, reps.csv.Bytes())
	checkJSONLReport(t, reps.jsonl.Bytes(), sampleCoverJSON)
	checkJSONLReport(t, reps.jsonlPrograms.Bytes(), sampleJSONLlProgs)
}

const kcovCode = `
#ifdef ASLR_BASE
#define _GNU_SOURCE
#endif

#include <stdio.h>

#ifdef ASLR_BASE
#include <dlfcn.h>
#include <link.h>
#include <stddef.h>

void* aslr_base() {
       struct link_map* map = NULL;
       void* handle = dlopen(NULL, RTLD_LAZY | RTLD_NOLOAD);
       if (handle != NULL) {
              dlinfo(handle, RTLD_DI_LINKMAP, &map);
              dlclose(handle);
       }
       return map ? (void *)map->l_addr : NULL;
}
#else
void* aslr_base() { return NULL; }
#endif

void __sanitizer_cov_trace_pc() { printf("%llu", (long long)(__builtin_return_address(0) - aslr_base())); }
`

func buildTestBinary(t *testing.T, target *targets.Target, test *Test, dir string) string {
	kcovSrc := filepath.Join(dir, "kcov.c")
	kcovObj := filepath.Join(dir, "kcov.o")
	if err := osutil.WriteFile(kcovSrc, []byte(kcovCode)); err != nil {
		t.Fatal(err)
	}

	aslrDefine := "-DNO_ASLR_BASE"
	if target.OS == targets.Linux || target.OS == targets.OpenBSD ||
		target.OS == targets.FreeBSD || target.OS == targets.NetBSD {
		aslrDefine = "-DASLR_BASE"
	}
	aslrExtraLibs := []string{}
	if target.OS == targets.Linux {
		aslrExtraLibs = []string{"-ldl"}
	}

	targetCFlags := slices.DeleteFunc(slices.Clone(target.CFlags), func(flag string) bool {
		return strings.HasPrefix(flag, "-std=c++")
	})
	kcovFlags := append([]string{"-c", "-fpie", "-w", "-x", "c", "-o", kcovObj, kcovSrc, aslrDefine}, targetCFlags...)
	src := filepath.Join(dir, "main.c")
	obj := filepath.Join(dir, "main.o")
	bin := filepath.Join(dir, target.KernelObject)
	if err := osutil.WriteFile(src, []byte(`int main() {}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := osutil.RunCmd(time.Hour, "", target.CCompiler, kcovFlags...); err != nil {
		t.Fatal(err)
	}

	// We used to compile and link with a single compiler invocation,
	// but clang has a bug that it tries to link in ubsan runtime when
	// -fsanitize-coverage=trace-pc is provided during linking and
	// ubsan runtime is missing for arm/arm64/riscv arches in the llvm packages.
	// So we first compile with -fsanitize-coverage and then link w/o it.
	cflags := append(append([]string{"-w", "-c", "-o", obj, src}, targetCFlags...), test.CFlags...)
	if test.DebugInfo {
		// TODO: pkg/cover doesn't support DWARF5 yet, which is the default in Clang.
		cflags = append([]string{"-g", "-gdwarf-4"}, cflags...)
	}
	if _, err := osutil.RunCmd(time.Hour, "", target.CCompiler, cflags...); err != nil {
		errText := err.Error()
		errText = strings.ReplaceAll(errText, "‘", "'")
		errText = strings.ReplaceAll(errText, "’", "'")
		if strings.Contains(errText, "error: unrecognized command line option '-fsanitize-coverage=trace-pc'") &&
			os.Getenv("SYZ_ENV") == "" {
			t.Skip("skipping test, -fsanitize-coverage=trace-pc is not supported")
		}
		t.Fatal(err)
	}

	ldflags := append(append(append([]string{"-o", bin, obj, kcovObj}, aslrExtraLibs...),
		targetCFlags...), test.LDFlags...)
	staticIdx, pieIdx := -1, -1
	for i, arg := range ldflags {
		switch arg {
		case "-static":
			staticIdx = i
		case "-pie":
			pieIdx = i
		}
	}
	if target.OS == targets.Fuchsia && pieIdx != -1 {
		// Fuchsia toolchain fails when given -pie:
		// clang-12: error: argument unused during compilation: '-pie'
		ldflags[pieIdx] = ldflags[len(ldflags)-1]
		ldflags = ldflags[:len(ldflags)-1]
	} else if pieIdx != -1 && staticIdx != -1 {
		// -static and -pie are incompatible during linking.
		ldflags[staticIdx] = ldflags[len(ldflags)-1]
		ldflags = ldflags[:len(ldflags)-1]
	}
	if _, err := osutil.RunCmd(time.Hour, "", target.CCompiler, ldflags...); err != nil {
		// Arm linker in the env image has a bug when linking a clang-produced files.
		if regexp.MustCompile(`arm-linux-gnueabi.* assertion fail`).MatchString(err.Error()) {
			t.Skipf("skipping test, broken arm linker (%v)", err)
		}
		t.Fatal(err)
	}
	return bin
}

type reports struct {
	csv           *bytes.Buffer
	jsonl         *bytes.Buffer
	jsonlPrograms *bytes.Buffer
}

func generateReport(t *testing.T, target *targets.Target, test *Test) (*reports, error) {
	dir := t.TempDir()
	bin := buildTestBinary(t, target, test, dir)
	cfg := &mgrconfig.Config{
		Derived: mgrconfig.Derived{
			SysTarget: target,
		},
		KernelObj:      dir,
		KernelSrc:      dir,
		KernelBuildSrc: dir,
		Type:           "",
	}
	cfg.KernelSubsystem = []mgrconfig.Subsystem{
		{
			Name: "sound",
			Paths: []string{
				"sound",
				"techpack/audio",
			},
		},
	}
	modules, err := backend.DiscoverModules(cfg.SysTarget, cfg.KernelObj, cfg.ModuleObj)
	if err != nil {
		return nil, err
	}

	// Deep copy, as we are going to modify progs. Our test generate multiple reports from the same
	// test object in parallel. Without copying we have a datarace here.
	progs := []Prog{}
	for _, p := range test.Progs {
		progs = append(progs, Prog{Sig: p.Sig, Data: p.Data, PCs: append([]uint64{}, p.PCs...)})
	}

	rg, err := MakeReportGenerator(cfg, modules)
	if err != nil {
		return nil, err
	}
	if !rg.PreciseCoverage && test.SkipIfKcovIsBroken {
		t.Skip("coverage testing requested, but kcov is broken")
	}
	if test.AddCover {
		var pcs []uint64
		Inexact := false
		// Sanitizers crash when installing signal handlers with static libc.
		const sanitizerOptions = "handle_segv=0:handle_sigbus=0:handle_sigfpe=0"
		cmd := osutil.Command(bin)
		cmd.Env = append([]string{
			"UBSAN_OPTIONS=" + sanitizerOptions,
			"ASAN_OPTIONS=" + sanitizerOptions,
		}, os.Environ()...)
		if output, err := osutil.Run(time.Minute, cmd); err == nil {
			pc, err := strconv.ParseUint(string(output), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			pcs = append(pcs, backend.PreviousInstructionPC(target, "", pc))
			t.Logf("using exact coverage PC 0x%x", pcs[0])
		} else if target.OS == runtime.GOOS && (target.Arch == runtime.GOARCH || target.VMArch == runtime.GOARCH) {
			t.Fatal(err)
		} else {
			text, err := symbolizer.ReadTextSymbols(bin)
			if err != nil {
				t.Fatal(err)
			}
			if nmain := len(text["main"]); nmain != 1 {
				t.Fatalf("got %v main symbols", nmain)
			}
			main := text["main"][0]
			for off := 0; off < main.Size; off++ {
				pcs = append(pcs, main.Addr+uint64(off))
			}
			t.Logf("using inexact coverage range 0x%x-0x%x", main.Addr, main.Addr+uint64(main.Size))
			Inexact = true
		}
		if Inexact && test.Result == "" && rg.PreciseCoverage {
			test.Result = fmt.Sprintf("%d out of %d PCs returned by kcov do not have matching coverage callbacks",
				len(pcs)-1, len(pcs))
		}
		if test.AddBadPc {
			pcs = append(pcs, 0xdeadbeef)
		}
		progs = append(progs, Prog{Data: "main", PCs: pcs})
	}
	params := HandlerParams{
		Progs: progs,
	}
	if err := rg.DoHTML(new(bytes.Buffer), params); err != nil {
		return nil, err
	}
	assert.NoError(t, rg.DoSubsystemCover(new(bytes.Buffer), params))
	assert.NoError(t, rg.DoFileCover(new(bytes.Buffer), params))
	res := &reports{
		csv:           new(bytes.Buffer),
		jsonl:         new(bytes.Buffer),
		jsonlPrograms: new(bytes.Buffer),
	}
	assert.NoError(t, rg.DoFuncCover(res.csv, params))
	assert.NoError(t, rg.DoCoverJSONL(res.jsonl, params))
	assert.NoError(t, rg.DoCoverPrograms(res.jsonlPrograms, params))
	return res, nil
}

func checkCSVReport(t *testing.T, CSVReport []byte) {
	csvReader := csv.NewReader(bytes.NewBuffer(CSVReport))
	lines, err := csvReader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(lines[0], csvHeader) {
		t.Fatalf("heading line in CSV doesn't match %v", lines[0])
	}

	foundMain := false
	for _, line := range lines {
		if line[2] == "main" {
			foundMain = true
			if line[3] != "1" && line[4] != "1" {
				t.Fatalf("function coverage percentage doesn't match %v vs. %v", line[3], "100")
			}
		}
	}
	if !foundMain {
		t.Fatalf("no main in the CSV report")
	}
}

// nolint:lll
func checkJSONLReport(t *testing.T, gotBytes, wantBytes []byte) {
	compacted := new(bytes.Buffer)
	if err := json.Compact(compacted, wantBytes); err != nil {
		t.Errorf("failed to prepare compacted json: %v", err)
	}
	compacted.Write([]byte("\n"))

	// PC is hard to predict here. Let's fix it.
	actualString := regexp.MustCompile(`"pc":[0-9]*`).ReplaceAllString(
		string(gotBytes), `"pc":12345`)
	assert.Equal(t, compacted.String(), actualString)
}

var sampleJSONLlProgs = []byte(`{
	"program": "main",
	"coverage": [
		{
			"file_path": "main.c",
			"functions": [
				{
					"func_name": "main",
					"covered_blocks": [
						{
							"from_line": 1,
							"from_column": 0,
							"to_line": 1,
							"to_column": -1
						}
					]
				}
			]
		}
	]
}`)

func makeFileStat(name string) fileStats {
	return fileStats{
		Name:                       name,
		CoveredLines:               1,
		TotalLines:                 8,
		CoveredPCs:                 1,
		TotalPCs:                   4,
		TotalFunctions:             2,
		CoveredFunctions:           1,
		CoveredPCsInFunctions:      1,
		TotalPCsInCoveredFunctions: 2,
		TotalPCsInFunctions:        2,
	}
}

func TestCoverByFilePrefixes(t *testing.T) {
	datas := []fileStats{
		makeFileStat("a"),
		makeFileStat("a/1"),
		makeFileStat("a/2"),
		makeFileStat("a/2/A"),
		makeFileStat("a/3"),
	}
	subsystems := []mgrconfig.Subsystem{
		{
			Name: "test",
			Paths: []string{
				"a",
				"-a/2",
			},
		},
	}
	d := groupCoverByFilePrefixes(datas, subsystems)
	assert.Equal(t, d["test"], map[string]string{
		"name":              "test",
		"lines":             "3 / 24 / 12.50%",
		"PCsInFiles":        "3 / 12 / 25.00%",
		"Funcs":             "3 / 6 / 50.00%",
		"PCsInFuncs":        "3 / 6 / 50.00%",
		"PCsInCoveredFuncs": "3 / 6 / 50.00%",
	})
}
