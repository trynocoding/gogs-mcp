// Command mkpackage writes the metadata artifacts of the offline binary
// bundle: SBOM, third-party licenses, source identity, and the package
// manifest. The Taskfile places every other package file before invoking it.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gogs-mcp/internal/packagegen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mkpackage: %s.\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("mkpackage", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("root", "", "Package directory that already contains bin, source, config, docs, and scripts.")
	project := flags.String("project", "", "Project root holding vendor/ and go.mod.")
	productVersion := flags.String("version", "", "Version injected into the packaged binary.")
	commit := flags.String("commit", "", "Commit injected into the packaged binary.")
	buildTime := flags.String("build-time", "unknown", "Build time injected into the binary.")
	gogsAPI := flags.String("gogs-api", "v0.14.2", "Target Gogs API version of the bundle.")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *root == "" || *project == "" || *productVersion == "" || *commit == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: mkpackage -root PACKAGE_DIR -project PROJECT_ROOT -version VERSION -commit COMMIT [--build-time TIME]")
	}

	binaryPath := filepath.Join(*root, "bin", "gogs-mcp")
	info, err := packagegen.InspectBinary(binaryPath)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", binaryPath, err)
	}
	if !info.Static {
		return fmt.Errorf("%s is not statically linked; build with CGO_ENABLED=0", binaryPath)
	}
	binaryDigest, err := packagegen.FileSHA256(binaryPath)
	if err != nil {
		return err
	}
	sourceFiles, err := filepath.Glob(filepath.Join(*root, "source", "gogs-mcp-*.tar.gz"))
	if err != nil || len(sourceFiles) != 1 {
		return fmt.Errorf("expected exactly one source archive under %s/source", *root)
	}
	sourceArchive := sourceFiles[0]
	sourceDigest, err := packagegen.FileSHA256(sourceArchive)
	if err != nil {
		return err
	}

	modules, err := readModules(*project)
	if err != nil {
		return err
	}

	sbom, err := packagegen.BuildSBOM("gogs-mcp", *productVersion, *commit, modules)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*root, "SBOM.cdx.json"), sbom, 0o644); err != nil {
		return err
	}
	licenses, err := packagegen.BuildLicensesText(filepath.Join(*project, "vendor"), modules)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*root, "THIRD_PARTY_LICENSES.txt"), licenses, 0o644); err != nil {
		return err
	}
	metadata, err := packagegen.BuildSourceMetadata(packagegen.SourceMetadata{
		Product:   "gogs-mcp",
		Version:   *productVersion,
		Commit:    *commit,
		BuildTime: *buildTime,
		GoVersion: runtime.Version(),
		Target:    packagegen.Target{OS: "linux", Arch: info.Arch},
		GogsAPI:   *gogsAPI,
		Binary:    packagegen.Artifact{Path: "bin/gogs-mcp", SHA256: binaryDigest},
		Source:    packagegen.Artifact{Path: "source/" + filepath.Base(sourceArchive), SHA256: sourceDigest},
		Static:    info.Static,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*root, "SOURCE-METADATA.json"), metadata, 0o644); err != nil {
		return err
	}
	return packagegen.WriteManifest(*root)
}

func readModules(project string) ([]packagegen.Module, error) {
	contents, err := os.ReadFile(filepath.Join(project, "vendor", "modules.txt"))
	if err != nil {
		return nil, fmt.Errorf("read vendor/modules.txt: %w", err)
	}
	modules := packagegen.ParseModulesTxt(string(contents))
	if len(modules) == 0 {
		return nil, fmt.Errorf("vendor/modules.txt lists no modules")
	}
	return modules, nil
}
