package pathmap

import (
	"errors"
	"path"
	"strings"
)

type Mapper struct {
	LocalBase   string
	VisibleBase string
}

func New(local, visible string) *Mapper {
	return &Mapper{
		LocalBase:   strings.TrimRight(local, "/"),
		VisibleBase: strings.TrimRight(visible, "/"),
	}
}

func (m *Mapper) Local(folderName string) (string, error) {
	clean, err := safeJoin(folderName)
	if err != nil {
		return "", err
	}
	return m.LocalBase + "/" + clean, nil
}

func (m *Mapper) Visible(folderName string) (string, error) {
	clean, err := safeJoin(folderName)
	if err != nil {
		return "", err
	}
	return m.VisibleBase + "/" + clean, nil
}

func safeJoin(folderName string) (string, error) {
	folderName = strings.TrimSpace(folderName)
	if folderName == "" {
		return "", errors.New("empty folder name")
	}
	if strings.ContainsAny(folderName, "/\\") {
		return "", errors.New("folder name must not contain path separators")
	}
	if folderName == "." || folderName == ".." {
		return "", errors.New("invalid folder name")
	}
	cleaned := path.Clean(folderName)
	if cleaned != folderName {
		return "", errors.New("folder name not in canonical form")
	}
	return cleaned, nil
}
