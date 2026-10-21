package util

import (
	"fmt"
	"strconv"
)

// StrIfMap is a convenient data type for dealing with maps of strings to any
type StrIfMap map[string]any

func getAs[T any](m StrIfMap, key, typeName string) (T, error) {
	var zero T
	ii, ok := m[key]
	if !ok {
		return zero, fmt.Errorf("No '%s' key in map", key)
	}
	val, ok := ii.(T)
	if !ok {
		return zero, fmt.Errorf("The '%s' key in map is not %s", key, typeName)
	}
	return val, nil
}

// GetString returns a string from this map.
func (m StrIfMap) GetString(key string) (string, error) {
	return getAs[string](m, key, "a string")
}

// GetInt64 returns an Int64 from this map.
func (m StrIfMap) GetInt64(key string) (int64, error) {
	val, err := getAs[float64](m, key, "an int64")
	return int64(val), err
}

// GetInt returns an int from this map.
func (m StrIfMap) GetInt(key string) (int, error) {
	val, err := getAs[float64](m, key, "an int")
	return int(val), err
}

// GetIntStringMap returns a map of integers to strings from this map.
func (m StrIfMap) GetIntStringMap(key string) (map[int]string, error) {
	val, err := getAs[map[string]any](m, key, "a map[string]any")
	if err != nil {
		return nil, err
	}
	result := make(map[int]string, len(val))
	for k, v := range val {
		i, err := strconv.Atoi(k)
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("The value in the hashes map is not a string?")
		}
		result[i] = s
	}
	return result, nil
}

// GetStringList returns a slice of strings from this map
func (m StrIfMap) GetStringList(key string) ([]string, error) {
	ilist, err := getAs[[]any](m, key, "an array of any")
	if err != nil {
		return nil, err
	}
	var result []string
	for n, is := range ilist {
		s, ok := is.(string)
		if !ok {
			return nil, fmt.Errorf("Element of %d the '%s' key is not a string", n, key)
		}
		result = append(result, s)
	}
	return result, nil
}
