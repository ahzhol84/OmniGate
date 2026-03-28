package common

import (
	"hash/fnv"
	"strconv"
	"strings"
)

func ResolveDeviceUniqueID(explicitUniqueID, companyName, pluginName, deviceType, sourceDeviceID string) string {
	explicitUniqueID = strings.TrimSpace(explicitUniqueID)
	if IsNumericDeviceID(explicitUniqueID) {
		return explicitUniqueID
	}

	seedParts := []string{companyName, pluginName, deviceType, sourceDeviceID}
	if explicitUniqueID != "" {
		seedParts = append(seedParts, explicitUniqueID)
	}

	parts := seedParts
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(part, "|", "_")
		cleaned = append(cleaned, part)
	}

	seed := strings.Join(cleaned, "|")
	if seed == "" {
		seed = "unknown-device"
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	hashValue := h.Sum64()

	const base uint64 = 100000000000000000
	const span uint64 = 900000000000000000
	numericID := base + (hashValue % span)
	return strconv.FormatUint(numericID, 10)
}

func IsNumericDeviceID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
