package protoregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/desc/protoparse"
)

func parseProtoFiles(importPaths []string) (fds []*desc.FileDescriptor, err error) {
	userDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get user home dir: %w", err)
	}

	var ip []string
	for _, importPath := range importPaths {
		if importPath == "~" {
			importPath = userDir
		} else if strings.HasPrefix(importPath, "~/") {
			importPath = filepath.Join(userDir, importPath[2:])
		}

		importPath, err = filepath.Abs(importPath)
		if err != nil {
			return nil, fmt.Errorf("getting absolute path for %q: %w", importPath, err)
		}

		if !strings.HasSuffix(importPath, "/") {
			importPath += "/"
		}
		ip = append(ip, importPath)
	}

	parser := protoparse.Parser{
		ImportPaths: ip,
	}

	var protoFiles []string
	for _, importPath := range ip {
		err := filepath.Walk(importPath,
			func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if strings.HasSuffix(path, ".proto") && !info.IsDir() {
					protoFiles = append(protoFiles, strings.TrimPrefix(path, importPath))
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("walking import path %q: %w", importPath, err)
		}
	}

	fds, err = parser.ParseFiles(protoFiles...)
	if err != nil {
		return nil, fmt.Errorf("parsing proto files: %w", err)
	}
	return

}
