package eval

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/bits"
	"strconv"

	"github.com/J-X0-cloud/flagpost/model"
)

// Bucket maps value to a bucket in [0, model.TotalWeight) using a hash
// seeded with seed. The mapping is stable across processes and platforms,
// which is what makes percentage rollouts consistent: a context keeps its
// bucket for as long as the seed is unchanged, and growing a rollout from
// 10% to 20% only adds contexts, it never reshuffles the existing 10%.
//
// The first 64 bits of a SHA-256 digest are scaled onto the bucket range
// with a 128-bit multiply rather than a modulo, which avoids the slight
// bias that x % n has when n does not divide 2^64.
func Bucket(seed, value string) int {
	h := sha256.New()
	h.Write([]byte(seed))
	h.Write([]byte{0}) // separator: ("ab","c") and ("a","bc") must differ
	h.Write([]byte(value))
	var sum [sha256.Size]byte
	x := binary.BigEndian.Uint64(h.Sum(sum[:0]))
	hi, _ := bits.Mul64(x, model.TotalWeight)
	return int(hi)
}

// bucketValue converts an attribute value into the string that is hashed.
// Strings hash as themselves and integral numbers hash as their decimal
// form, so an attribute sent as 42 or "42" lands in the same bucket.
// Other types cannot be bucketed.
func bucketValue(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1<<53 {
			return strconv.FormatInt(int64(t), 10), true
		}
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// pickVariant walks the cumulative weights and returns the slice containing
// bucket. Zero-weight slices are skipped so that they can never be chosen,
// even at the boundary.
func pickVariant(r *model.Rollout, bucket int) (string, bool) {
	cum := 0
	last := ""
	for _, wv := range r.Variants {
		if wv.Weight <= 0 {
			continue
		}
		cum += wv.Weight
		last = wv.Variant
		if bucket < cum {
			return wv.Variant, true
		}
	}
	// Weights that sum to less than the total leave the top of the range
	// unassigned. Validation rejects that, but snapshots can come from
	// older servers, so fall back to the last positive slice.
	return last, last != ""
}
