package util

// ToPointers converts a slice of values to a slice of pointers.
func ToPointers[T any](in []T) []*T {
	out := make([]*T, len(in))
	for i := range in {
		out[i] = &in[i]
	}
	return out
}

func ReorderToFront[T comparable](frontMap map[T]bool, inputSlice []T) []T {
	var prioritized []T
	var remaining []T

	for _, item := range inputSlice {
		if frontMap[item] {
			prioritized = append(prioritized, item)
		} else {
			remaining = append(remaining, item)
		}
	}

	return append(prioritized, remaining...)
}

// DrainChannel performs a non-blocking drain of a channel after receiving
// the first message. Returns all drained messages including the first one.
// The first message is passed in explicitly (already received by the caller).
func DrainChannel[T any](ch chan T, first T) []T {
	result := []T{first}
	for {
		select {
		case v := <-ch:
			result = append(result, v)
		default:
			return result
		}
	}
}
