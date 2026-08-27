package update

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DefaultVersionURL = "https://download.htmlclay.com/htmlclay-release-info.json"

type Info struct {
	Version string `json:"latest"`
	URL     string `json:"url"`
}

func Check(currentVersion, versionURL string) *Info {
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(versionURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil
	}

	if info.Version == "" || info.Version == currentVersion {
		return nil
	}

	if compareVersions(info.Version, currentVersion) > 0 {
		return &info
	}

	return nil
}

func compareVersions(a, b string) int {
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}

	for i := 0; i < maxLen; i++ {
		var numA, numB int
		if i < len(partsA) {
			numA, _ = strconv.Atoi(partsA[i])
		}
		if i < len(partsB) {
			numB, _ = strconv.Atoi(partsB[i])
		}
		if numA > numB {
			return 1
		}
		if numA < numB {
			return -1
		}
	}
	return 0
}
