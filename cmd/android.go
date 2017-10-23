// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cmd

import (
	"archive/zip"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	javacTargetVer = "1.7"
	minAndroidAPI  = 15
)

const manifestHeader = `Manifest-Version: 1.0
Created-By: 1.0 (Go)

`

func validateAndroidInstall() error {
	if _, err := AndroidAPIPath(); err != nil {
		fmt.Println("Android SDK could not be found. Install Android SDK or run with --target='ios' to not build for Android.")
		return err
	}
	if _, err := ndkRoot(); err != nil {
		fmt.Println("Android NDK could not be found. Install Android NDK or run with --target='ios' to not build for Android.")
		return err
	}
	return nil
}

func androidEnv(goarch string) ([]string, error) {
	tc, err := toolchainForArch(goarch)
	if err != nil {
		return nil, err
	}
	flags := fmt.Sprintf("-target %s -gcc-toolchain %s", tc.clangTriple, tc.gccToolchain())
	cflags := fmt.Sprintf("%s --sysroot %s -isystem %s -D__ANDROID_API__=%s", flags, tc.csysroot(), tc.isystem(), tc.api)
	ldflags := fmt.Sprintf("%s --sysroot %s", flags, tc.ldsysroot())
	env := []string{
		"GOOS=android",
		"GOARCH=" + goarch,
		"CC=" + tc.clangPath(),
		"CXX=" + tc.clangppPath(),
		"CGO_CFLAGS=" + cflags,
		"CGO_CPPFLAGS=" + cflags,
		"CGO_LDFLAGS=" + ldflags,
		"CGO_ENABLED=1",
	}
	if goarch == "arm" {
		env = append(env, "GOARM=7")
	}
	return env, nil
}

func ndkHostTag() (string, error) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "386" {
		return "windows", nil
	} else {
		var arch string
		switch runtime.GOARCH {
		case "386":
			arch = "x86"
		case "amd64":
			arch = "x86_64"
		default:
			return "", fmt.Errorf("ndkHostTag(): Unsupported GOARCH %v", runtime.GOARCH)
		}
		return runtime.GOOS + "-" + arch, nil
	}
}

func ndkRoot() (string, error) {
	sdkHome := os.Getenv("ANDROID_HOME")
	if sdkHome == "" {
		return "", fmt.Errorf("ndkRoot(): $ANDROID_HOME enviromental var is unset.")
	}

	path, err := filepath.Abs(filepath.Join(sdkHome, "ndk-bundle"))
	if err != nil {
		return "", fmt.Errorf("ndkRoot(): Error cleaning path %v.", err)
	}

	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return "", fmt.Errorf("ndkRoot(): Missing $ANDROID_HOME/ndk-bundle directory at %v.", path)
	}
	return path, nil
}

// Emulate the flags in the clang wrapper scripts generated
// by make_standalone_toolchain.py
// https://android.googlesource.com/platform/ndk/+/ndk-release-r16/docs/UnifiedHeaders.md
// https://developer.android.com/ndk/guides/standalone_toolchain.html#c_stl_support
// http://zwyuan.github.io/2015/12/22/three-ways-to-use-android-ndk-cross-compiler/
type ndkToolchain struct {
	arch        string
	api         string
	gcc         string
	triple      string
	clangTriple string

	ndkRoot string
	hostTag string
}

func toolchainForArch(goarch string) (*ndkToolchain, error) {
	m := map[string]*ndkToolchain{
		"arm": &ndkToolchain{
			arch:        "arm",
			api:         "15",
			gcc:         "arm-linux-androideabi-4.9",
			triple:      "arm-linux-androideabi",
			clangTriple: "armv7a-none-linux-androideabi",
		},
		"arm64": &ndkToolchain{
			arch:        "arm64",
			api:         "21",
			gcc:         "aarch64-linux-android-4.9",
			triple:      "aarch64-linux-android",
			clangTriple: "aarch64-none-linux-android",
		},
		"386": &ndkToolchain{
			arch:        "x86",
			api:         "15",
			gcc:         "x86-4.9",
			triple:      "i686-linux-android",
			clangTriple: "i686-none-linux-android",
		},
		"amd64": &ndkToolchain{
			arch:        "x86_64",
			api:         "21",
			gcc:         "x86_64-4.9",
			triple:      "x86_64-linux-android",
			clangTriple: "x86_64-none-linux-android",
		},
	}
	toolchain, ok := m[goarch]
	if !ok {
		return nil, fmt.Errorf("toolchainForArch(): Unknown arch %v", goarch)
	}

	ndkRoot, err := ndkRoot()
	if err != nil {
		return nil, err
	}
	toolchain.ndkRoot = ndkRoot

	hostTag, err := ndkHostTag()
	if err != nil {
		return nil, err
	}
	toolchain.hostTag = hostTag
	return toolchain, nil
}

func (tc *ndkToolchain) gccToolchain() string {
	return filepath.Join(tc.ndkRoot, "toolchains", tc.gcc, "prebuilt", tc.hostTag)
}

func (tc *ndkToolchain) clangPath() string {
	return filepath.Join(tc.ndkRoot, "toolchains", "llvm", "prebuilt", tc.hostTag, "bin", "clang")
}

func (tc *ndkToolchain) clangppPath() string {
	return filepath.Join(tc.ndkRoot, "toolchains", "llvm", "prebuilt", tc.hostTag, "bin", "clang++")
}

func (tc *ndkToolchain) isystem() string {
	return filepath.Join(tc.ndkRoot, "sysroot", "usr", "include", tc.triple)
}

func (tc *ndkToolchain) csysroot() string {
	return filepath.Join(tc.ndkRoot, "sysroot")
}

func (tc *ndkToolchain) ldsysroot() string {
	return filepath.Join(tc.ndkRoot, "platforms", "android-"+tc.api, "arch-"+tc.arch)
}

func GetAndroidABI(arch string) string {
	switch arch {
	case "arm":
		return "armeabi-v7a"
	case "arm64":
		return "arm64-v8a"
	case "386":
		return "x86"
	case "amd64":
		return "x86_64"
	}
	return ""
}

// androidAPIPath returns an android SDK platform directory under ANDROID_HOME.
// If there are multiple platforms that satisfy the minimum version requirement
// androidAPIPath returns the latest one among them.
func AndroidAPIPath() (string, error) {
	sdk := os.Getenv("ANDROID_HOME")
	if sdk == "" {
		return "", fmt.Errorf("ANDROID_HOME environment var is not set")
	}
	sdkDir, err := os.Open(filepath.Join(sdk, "platforms"))
	if err != nil {
		return "", fmt.Errorf("failed to find android SDK platform: %v", err)
	}
	defer sdkDir.Close()
	fis, err := sdkDir.Readdir(-1)
	if err != nil {
		return "", fmt.Errorf("failed to find android SDK platform (min API level: %d): %v", minAndroidAPI, err)
	}

	var apiPath string
	var apiVer int
	for _, fi := range fis {
		name := fi.Name()
		if !fi.IsDir() || !strings.HasPrefix(name, "android-") {
			continue
		}
		n, err := strconv.Atoi(name[len("android-"):])
		if err != nil || n < minAndroidAPI {
			continue
		}
		p := filepath.Join(sdkDir.Name(), name)
		_, err = os.Stat(filepath.Join(p, "android.jar"))
		if err == nil && apiVer < n {
			apiPath = p
			apiVer = n
		}
	}
	if apiVer == 0 {
		return "", fmt.Errorf("failed to find android SDK platform (min API level: %d) in %s",
			minAndroidAPI, sdkDir.Name())
	}
	return apiPath, nil
}

// AAR is the format for the binary distribution of an Android Library Project
// and it is a ZIP archive with extension .aar.
// http://tools.android.com/tech-docs/new-build-system/aar-format
//
// These entries are directly at the root of the archive.
//
//  AndroidManifest.xml (mandatory)
//  classes.jar (mandatory)
//  assets/ (optional)
//  jni/<abi>/libgojni.so
//  R.txt (mandatory)
//  res/ (mandatory)
//  libs/*.jar (optional, not relevant)
//  proguard.txt (optional)
//  lint.jar (optional, not relevant)
//  aidl (optional, not relevant)
//
// javac and jar commands are needed to build classes.jar.
func BuildAAR(flags *Flags, androidDir string, pkgs []*build.Package, androidArchs []string, tmpdir string, aarPath string) (err error) {
	var out io.Writer = ioutil.Discard
	if !flags.BuildN {
		f, err := os.Create(aarPath)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}()
		out = f
	}

	aarw := zip.NewWriter(out)
	aarwcreate := func(name string) (io.Writer, error) {
		if flags.BuildV {
			fmt.Fprintf(os.Stderr, "aar: %s\n", name)
		}
		return aarw.Create(name)
	}
	w, err := aarwcreate("AndroidManifest.xml")
	if err != nil {
		return err
	}
	const manifestFmt = `<manifest xmlns:android="http://schemas.android.com/apk/res/android" package=%q>
<uses-sdk android:minSdkVersion="%d"/></manifest>`
	fmt.Fprintf(w, manifestFmt, "go."+pkgs[0].Name+".gojni", minAndroidAPI)

	w, err = aarwcreate("proguard.txt")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, `-keep class go.** { *; }`)

	w, err = aarwcreate("classes.jar")
	if err != nil {
		return err
	}
	src := filepath.Join(androidDir, "src/main/java")
	if err := BuildJar(flags, w, src, tmpdir); err != nil {
		return err
	}

	files := map[string]string{}
	for _, pkg := range pkgs {
		assetsDir := filepath.Join(pkg.Dir, "assets")
		assetsDirExists := false
		if fi, err := os.Stat(assetsDir); err == nil {
			assetsDirExists = fi.IsDir()
		} else if !os.IsNotExist(err) {
			return err
		}

		if assetsDirExists {
			err := filepath.Walk(
				assetsDir, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if info.IsDir() {
						return nil
					}
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					defer f.Close()
					name := "assets/" + path[len(assetsDir)+1:]
					if orig, exists := files[name]; exists {
						return fmt.Errorf("package %s asset name conflict: %s already added from package %s",
							pkg.ImportPath, name, orig)
					}
					files[name] = pkg.ImportPath
					w, err := aarwcreate(name)
					if err != nil {
						return nil
					}
					_, err = io.Copy(w, f)
					return err
				})
			if err != nil {
				return err
			}
		}
	}

	for _, arch := range androidArchs {
		lib := GetAndroidABI(arch) + "/libgojni.so"
		w, err = aarwcreate("jni/" + lib)
		if err != nil {
			return err
		}
		if !flags.BuildN {
			r, err := os.Open(filepath.Join(androidDir, "src/main/jniLibs/"+lib))
			if err != nil {
				return err
			}
			defer r.Close()
			if _, err := io.Copy(w, r); err != nil {
				return err
			}
		}
	}

	// TODO(hyangah): do we need to use aapt to create R.txt?
	w, err = aarwcreate("R.txt")
	if err != nil {
		return err
	}

	w, err = aarwcreate("res/")
	if err != nil {
		return err
	}

	return aarw.Close()
}

func BuildJar(flags *Flags, w io.Writer, srcDir string, tmpdir string) error {
	bindClasspath := ""

	var srcFiles []string
	if flags.BuildN {
		srcFiles = []string{"*.java"}
	} else {
		err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if filepath.Ext(path) == ".java" {
				srcFiles = append(srcFiles, filepath.Join(".", path[len(srcDir):]))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	dst := filepath.Join(tmpdir, "javac-output")
	if !flags.BuildN {
		if err := os.MkdirAll(dst, 0700); err != nil {
			return err
		}
	}

	bClspath, err := bootClasspath()

	if err != nil {
		return err
	}

	args := []string{
		"-d", dst,
		"-source", javacTargetVer,
		"-target", javacTargetVer,
		"-bootclasspath", bClspath,
	}
	if bindClasspath != "" {
		args = append(args, "-classpath", bindClasspath)
	}

	args = append(args, srcFiles...)

	javac := exec.Command("javac", args...)
	javac.Dir = srcDir
	if err := RunCmd(&Flags{}, tmpdir, javac); err != nil {
		return err
	}

	// fmt.Println("javac", args)
	// if buildX {
	// KD: printcmd("jar c -C %s .", dst)
	// }
	if flags.BuildN {
		return nil
	}
	jarw := zip.NewWriter(w)
	jarwcreate := func(name string) (io.Writer, error) {
		if flags.BuildV {
			fmt.Fprintf(os.Stderr, "jar: %s\n", name)
		}
		return jarw.Create(name)
	}
	f, err := jarwcreate("META-INF/MANIFEST.MF")
	if err != nil {
		return err
	}
	fmt.Fprintf(f, manifestHeader)

	err = filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		out, err := jarwcreate(filepath.ToSlash(path[len(dst)+1:]))
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		return err
	}
	return jarw.Close()
}

func bootClasspath() (string, error) {
	// bindBootClasspath := "" // KD: command parameter
	// if bindBootClasspath != "" {
	// 	return bindBootClasspath, nil
	// }
	apiPath, err := AndroidAPIPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(apiPath, "android.jar"), nil
}