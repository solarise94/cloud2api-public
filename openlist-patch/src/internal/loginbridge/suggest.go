package loginbridge

import (
	"errors"
	"strconv"
	"strings"
	"unicode"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"gorm.io/gorm"
)

// suggestName strips "/" and control characters from a platform name.
func suggestName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '/' || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// SuggestMount proposes a mount path for a new storage of the platform name:
// "/<name>" plus "-2" … "-99" while taken, each candidate cleaned with the
// same rules as Create. It returns "" when no candidate is free.
func SuggestMount(name string) (string, error) {
	base := suggestName(name)
	for i := 1; i <= 99; i++ {
		raw := "/" + base
		if i > 1 {
			raw += "-" + strconv.Itoa(i)
		}
		mount, problem := cleanMountPath(raw)
		if problem != "" {
			return "", nil
		}
		_, err := db.GetStorageByMountPath(mount)
		if err == nil {
			continue // taken
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", err
		}
		return mount, nil
	}
	return "", nil
}
