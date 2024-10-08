package bindings

import (
	"encoding/json"

	"github.com/piplabs/story/lib/solc"
)

func mustGetStorageLayout(data []byte) solc.StorageLayout {
	var layout solc.StorageLayout
	if err := json.Unmarshal(data, &layout); err != nil {
		panic(err)
	}

	return layout
}
