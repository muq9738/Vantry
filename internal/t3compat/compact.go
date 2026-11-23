package t3compat

import "reflect"

// compactThreadProjection implements the opt-in turnItemsOmitLocalVisible wire
// format. The client prepends local visible items to turnItems when restoring,
// so only an equal, ordered prefix can be omitted. DeepEqual checks the JSON
// content too; matching IDs alone would hide a stale or different item body.
// project shares each item between both collections, making these comparisons
// cheap. Neither the authoritative projection nor its arrays are mutated.
func compactThreadProjection(projection Object) (Object, bool) {
	items, ok := projection["turnItems"].([]any)
	if !ok {
		return projection, false
	}
	visible, ok := projection["visibleTurnItems"].([]any)
	if !ok {
		return projection, false
	}
	threadID := text(object(projection["thread"]), "id")
	if threadID == "" {
		return projection, false
	}
	omitted := 0
	for _, value := range visible {
		row := object(value)
		if row["visibility"] != "local" && row["sourceThreadId"] != threadID {
			continue
		}
		item, ok := row["item"].(Object)
		if !ok || omitted >= len(items) || !reflect.DeepEqual(items[omitted], item) {
			return projection, false
		}
		omitted++
	}
	if omitted == 0 {
		return projection, false
	}
	compact := make(Object, len(projection))
	for key, value := range projection {
		compact[key] = value
	}
	compact["turnItems"] = items[omitted:]
	return compact, true
}
