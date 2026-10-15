package loginbridge

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// numberItemTypes are the driver form item types whose default is a number.
var numberItemTypes = map[string]bool{
	"number":  true,
	"int":     true,
	"int64":   true,
	"float64": true,
}

// itemDefault returns the typed default of one driver form item, the way the
// native storage form would submit it.
func itemDefault(item driver.Item) (any, error) {
	switch {
	case item.Type == "bool":
		return item.Default == "true", nil
	case numberItemTypes[item.Type]:
		return numberDefault(item.Default, item.Name)
	default:
		return item.Default, nil
	}
}

func numberDefault(s, name string) (any, error) {
	if s == "" {
		return int64(0), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("字段 %s 的默认值 %q 不是数字", name, s)
	}
	if f == math.Trunc(f) {
		return int64(f), nil
	}
	return f, nil
}

func itemDefaults(items []driver.Item) (map[string]any, error) {
	out := make(map[string]any, len(items))
	for _, item := range items {
		v, err := itemDefault(item)
		if err != nil {
			return nil, err
		}
		out[item.Name] = v
	}
	return out, nil
}

// buildAddition renders the addition JSON of a new storage: typed driver
// defaults, overlaid with the login's create-only fields, then its credential
// fields, then the chosen root.
func buildAddition(items []driver.Item, createFields, fields map[string]any, rootField, root string) (string, error) {
	m, err := itemDefaults(items)
	if err != nil {
		return "", err
	}
	for k, v := range createFields {
		m[k] = v
	}
	for k, v := range fields {
		m[k] = v
	}
	m[rootField] = root
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// buildStorage renders the common storage columns from the driver's common
// form items. model.Storage's json tags match the item names, so the map is
// round-tripped through JSON instead of enumerating the columns here.
func buildStorage(info driver.Info, mount, remark string) (*model.Storage, error) {
	m, err := itemDefaults(info.Common)
	if err != nil {
		return nil, err
	}
	m["mount_path"] = mount
	m["order"] = int64(0)
	m["remark"] = remark
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	st := &model.Storage{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	return st, nil
}
