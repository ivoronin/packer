// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:generate packer-sdc mapstructure-to-hcl2 -type Config

package compress

import (
	"archive/tar"
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/biogo/hts/bgzf"
	"github.com/dsnet/compress/bzip2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/common"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	"github.com/klauspost/pgzip"
	"github.com/pierrec/lz4"
	"github.com/ulikunitz/xz"
)

var (
	// ErrInvalidCompressionLevel is returned when the compression level passed
	// to gzip is not in the expected range. See compress/flate for details.
	ErrInvalidCompressionLevel = fmt.Errorf(
		"Invalid compression level. Expected an integer from -1 to 9.")

	ErrWrongInputCount = fmt.Errorf(
		"Can only have 1 input file when not using tar/zip")

	filenamePattern = regexp.MustCompile(`(?:\.([a-z0-9]+))`)
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// Fields from config file
	OutputPath       string `mapstructure:"output"`
	Format           string `mapstructure:"format"`
	CompressionLevel int    `mapstructure:"compression_level"`

	// Derived fields
	Archive   string
	Algorithm string

	ctx interpolate.Context
}

type PostProcessor struct {
	config Config
}

func (p *PostProcessor) ConfigSpec() hcldec.ObjectSpec { return p.config.FlatMapstructure().HCL2Spec() }

func (p *PostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		PluginType:         "compress",
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{"output"},
		},
	}, raws...)
	if err != nil {
		return err
	}

	errs := new(packersdk.MultiError)

	// If there is no explicit number of Go threads to use, then set it
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	if p.config.OutputPath == "" {
		p.config.OutputPath = "packer_{{.BuildName}}_{{.BuilderType}}"
	}

	if p.config.CompressionLevel > pgzip.BestCompression {
		p.config.CompressionLevel = pgzip.BestCompression
	}
	// Technically 0 means "don't compress" but I don't know how to
	// differentiate between "user entered zero" and "user entered nothing".
	// Also, why bother creating a compressed file with zero compression?
	if p.config.CompressionLevel == -1 || p.config.CompressionLevel == 0 {
		p.config.CompressionLevel = pgzip.DefaultCompression
	}

	if err = interpolate.Validate(p.config.OutputPath, &p.config.ctx); err != nil {
		errs = packersdk.MultiErrorAppend(
			errs, fmt.Errorf("Error parsing target template: %s", err))
	}

	p.config.detectFromFilename()

	if len(errs.Errors) > 0 {
		return errs
	}

	return nil
}

func (p *PostProcessor) PostProcess(
	ctx context.Context,
	ui packersdk.Ui,
	artifact packersdk.Artifact,
) (packersdk.Artifact, bool, bool, error) {
	var generatedData map[interface{}]interface{}
	stateData := artifact.State("generated_data")
	if stateData != nil {
		// Make sure it's not a nil map so we can assign to it later.
		generatedData = stateData.(map[interface{}]interface{})
	}
	// If stateData has a nil map generatedData will be nil
	// and we need to make sure it's not
	if generatedData == nil {
		generatedData = make(map[interface{}]interface{})
	}

	// These are extra variables that will be made available for interpolation.
	generatedData["BuildName"] = p.config.PackerBuildName
	generatedData["BuilderType"] = p.config.PackerBuilderType
	p.config.ctx.Data = generatedData

	target, err := interpolate.Render(p.config.OutputPath, &p.config.ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("Error interpolating output value: %s", err)
	} else {
		fmt.Println(target)
	}

	newArtifact := &Artifact{Path: target}

	if err = os.MkdirAll(filepath.Dir(target), os.FileMode(0755)); err != nil {
		return nil, false, false, fmt.Errorf(
			"Unable to create dir for archive %s: %s", target, err)
	}
	outputFile, err := os.Create(target)
	if err != nil {
		return nil, false, false, fmt.Errorf(
			"Unable to create archive %s: %s", target, err)
	}
	defer outputFile.Close()

	// Setup output interface. If we're using compression, output is a
	// compression writer. Otherwise it's just a file.
	var output io.WriteCloser
	errTmpl := "error creating %s writer: %s"
	switch p.config.Algorithm {
	case "bgzf":
		ui.Say(fmt.Sprintf("Using bgzf compression with %d cores for %s",
			runtime.GOMAXPROCS(-1), target))
		output, err = makeBGZFWriter(outputFile, p.config.CompressionLevel)
		if err != nil {
			return nil, false, false, fmt.Errorf(errTmpl, p.config.Algorithm, err)
		}
		defer output.Close()
	case "bzip2":
		ui.Say(fmt.Sprintf("Using bzip2 compression with 1 core for %s (library does not support MT)",
			target))
		output, err = makeBZIP2Writer(outputFile, p.config.CompressionLevel)
		if err != nil {
			return nil, false, false, fmt.Errorf(errTmpl, p.config.Algorithm, err)
		}
		defer output.Close()
	case "lz4":
		ui.Say(fmt.Sprintf("Using lz4 compression with %d cores for %s",
			runtime.GOMAXPROCS(-1), target))
		output, err = makeLZ4Writer(outputFile, p.config.CompressionLevel)
		if err != nil {
			return nil, false, false, fmt.Errorf(errTmpl, p.config.Algorithm, err)
		}
		defer output.Close()
	case "xz":
		ui.Say(fmt.Sprintf("Using xz compression with 1 core for %s (library does not support MT)",
			target))
		output, err = makeXZWriter(outputFile)
		if err != nil {
			return nil, false, false, fmt.Errorf(errTmpl, p.config.Algorithm, err)
		}
		defer output.Close()
	case "pgzip":
		ui.Say(fmt.Sprintf("Using pgzip compression with %d cores for %s",
			runtime.GOMAXPROCS(-1), target))
		output, err = makePgzipWriter(outputFile, p.config.CompressionLevel)
		if err != nil {
			return nil, false, false,
				fmt.Errorf(errTmpl, p.config.Algorithm, err)
		}
		defer output.Close()
	default:
		output = outputFile
	}

	compression := p.config.Algorithm
	if compression == "" {
		compression = "no compression"
	}

	// Build an archive, if we're supposed to do that.
	switch p.config.Archive {
	case "tar":
		ui.Say(fmt.Sprintf("Tarring %s with %s", target, compression))
		err = createTarArchive(artifact.Files(), output)
		if err != nil {
			return nil, false, false, fmt.Errorf("Error creating tar: %s", err)
		}
	case "zip":
		ui.Say(fmt.Sprintf("Zipping %s", target))
		err = createZipArchive(artifact.Files(), output)
		if err != nil {
			return nil, false, false, fmt.Errorf("Error creating zip: %s", err)
		}
	default:
		// Filename indicates no tarball (just compress) so we'll do an io.Copy
		// into our compressor.
		if len(artifact.Files()) != 1 {
			return nil, false, false, fmt.Errorf(
				"Can only have 1 input file when not using tar/zip. Found %d "+
					"files: %v", len(artifact.Files()), artifact.Files())
		}
		archiveFile := artifact.Files()[0]
		ui.Say(fmt.Sprintf("Archiving %s with %s", archiveFile, compression))

		source, err := os.Open(archiveFile)
		if err != nil {
			return nil, false, false, fmt.Errorf(
				"Failed to open source file %s for reading: %s",
				archiveFile, err)
		}
		defer source.Close()

		if _, err = io.Copy(output, source); err != nil {
			return nil, false, false, fmt.Errorf("Failed to compress %s: %s",
				archiveFile, err)
		}
	}

	ui.Say(fmt.Sprintf("Archive %s completed", target))

	return newArtifact, false, false, nil
}

func (config *Config) detectFromFilename() {
	var result [][]string

	extensions := map[string]string{
		"tar":   "tar",
		"zip":   "zip",
		"gz":    "pgzip",
		"lz4":   "lz4",
		"bgzf":  "bgzf",
		"xz":    "xz",
		"bzip2": "bzip2",
	}

	if config.Format == "" {
		result = filenamePattern.FindAllStringSubmatch(config.OutputPath, -1)
	} else {
		result = filenamePattern.FindAllStringSubmatch(fmt.Sprintf("%s.%s", config.OutputPath, config.Format), -1)
	}

	// No dots. Bail out with defaults.
	if len(result) == 0 {
		config.Algorithm = "pgzip"
		config.Archive = "tar"
		return
	}

	// Parse the last two .groups, if they're there
	lastItem := result[len(result)-1][1]
	var nextToLastItem string
	if len(result) == 1 {
		nextToLastItem = ""
	} else {
		nextToLastItem = result[len(result)-2][1]
	}

	// Should we make an archive? E.g. tar or zip?
	if nextToLastItem == "tar" {
		config.Archive = "tar"
	}
	if lastItem == "zip" || lastItem == "tar" {
		config.Archive = lastItem
		// Tar or zip is our final artifact. Bail out.
		return
	}

	// Should we compress the artifact?
	algorithm, ok := extensions[lastItem]
	if ok {
		config.Algorithm = algorithm
		// We found our compression algorithm. Bail out.
		return
	}

	// We didn't match a known compression format. Default to tar + pgzip
	config.Algorithm = "pgzip"
	config.Archive = "tar"
	return
}

func makeBGZFWriter(output io.WriteCloser, compressionLevel int) (io.WriteCloser, error) {
	bgzfWriter, err := bgzf.NewWriterLevel(output, compressionLevel, runtime.GOMAXPROCS(-1))
	if err != nil {
		return nil, ErrInvalidCompressionLevel
	}
	return bgzfWriter, nil
}

func makeBZIP2Writer(output io.Writer, compressionLevel int) (io.WriteCloser, error) {
	// Set the default to highest level compression
	bzipCFG := &bzip2.WriterConfig{Level: 9}
	// Override our set defaults
	if compressionLevel > 0 {
		bzipCFG.Level = compressionLevel
	}
	bzipWriter, err := bzip2.NewWriter(output, bzipCFG)
	if err != nil {
		return nil, err
	}
	return bzipWriter, nil
}

func makeLZ4Writer(output io.WriteCloser, compressionLevel int) (io.WriteCloser, error) {
	lzwriter := lz4.NewWriter(output)
	if compressionLevel > 0 {
		lzwriter.Header.CompressionLevel = compressionLevel
	}
	return lzwriter, nil
}

func makeXZWriter(output io.WriteCloser) (io.WriteCloser, error) {
	xzwriter, err := xz.NewWriter(output)
	if err != nil {
		return nil, err
	}
	return xzwriter, nil
}

func makePgzipWriter(output io.WriteCloser, compressionLevel int) (io.WriteCloser, error) {
	gzipWriter, err := pgzip.NewWriterLevel(output, compressionLevel)
	if err != nil {
		return nil, ErrInvalidCompressionLevel
	}
	gzipWriter.SetConcurrency(500000, runtime.GOMAXPROCS(-1))
	return gzipWriter, nil
}

func createTarArchive(files []string, output io.WriteCloser) error {
	archive := tar.NewWriter(output)
	defer archive.Close()

	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("Unable to read file %s: %s", path, err)
		}
		defer file.Close()

		fi, err := file.Stat()
		if err != nil {
			return fmt.Errorf("Unable to get fileinfo for %s: %s", path, err)
		}

		header, err := tar.FileInfoHeader(fi, path)
		if err != nil {
			return fmt.Errorf("Failed to create tar header for %s: %s", path, err)
		}

		// workaround for archive format on go >=1.10
		setHeaderFormat(header)

		if err := archive.WriteHeader(header); err != nil {
			return fmt.Errorf("Failed to write tar header for %s: %s", path, err)
		}

		if _, err := io.Copy(archive, file); err != nil {
			return fmt.Errorf("Failed to copy %s data to archive: %s", path, err)
		}
	}
	return nil
}

func createZipArchive(files []string, output io.WriteCloser) error {
	archive := zip.NewWriter(output)
	defer archive.Close()

	for _, path := range files {
		path = filepath.ToSlash(path)

		source, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("Unable to read file %s: %s", path, err)
		}
		defer source.Close()

		target, err := archive.Create(path)
		if err != nil {
			return fmt.Errorf("Failed to add zip header for %s: %s", path, err)
		}

		_, err = io.Copy(target, source)
		if err != nil {
			return fmt.Errorf("Failed to copy %s data to archive: %s", path, err)
		}
	}
	return nil
}
