package hlltc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

const (
	capacity = uint8(16)
	pp       = uint8(25)
	mp       = uint32(1) << pp
	version  = 1
)

// Sketch ...
type Sketch struct {
	regs       *registers
	m          uint32
	p          uint8
	b          uint8
	alpha      float64
	sparse     bool
	sparseList *compressedList
	tmpSet     set
	hash       func(e []byte) uint64
}

// New ...
func New(precision uint8) (*Sketch, error) {
	if precision < 4 || precision > 18 {
		return nil, fmt.Errorf("p has to be >= 4 and <= 18")
	}
	m := uint32(math.Pow(2, float64(precision)))
	return &Sketch{
		m:          m,
		p:          precision,
		alpha:      alpha(float64(m)),
		sparse:     true,
		tmpSet:     set{},
		sparseList: newCompressedList(int(m)),
		hash:       hash,
	}, nil
}

// Merge takes another HyperLogLogPlus and combines it with HyperLogLogPlus h.
// If HyperLogLogPlus h is using the sparse representation, it will be converted
// to the normal representation.
func (sk *Sketch) Merge(other *Sketch) error {
	if other == nil {
		// Nothing to do
		return nil
	}

	if sk.p != other.p {
		return errors.New("precisions must be equal")
	}

	if sk.sparse {
		sk.toNormal()
	}

	if other.sparse {
		for k := range other.tmpSet {
			i, r := decodeHash(k, other.p, pp)
			if sk.regs.get(i) < r {
				sk.regs.set(i, r)
			}
		}

		for iter := other.sparseList.Iter(); iter.HasNext(); {
			i, r := decodeHash(iter.Next(), other.p, pp)
			if sk.regs.get(i) < r {
				sk.regs.set(i, r)
			}
		}
	} else {
		for i, v := range other.regs.fields {
			v1 := v.get(0)
			if v1 > sk.regs.get(uint32(i)*2) {
				sk.regs.set(uint32(i)*2, v1)
			}
			v2 := v.get(1)
			if v2 > sk.regs.get(1+uint32(i)*2) {
				sk.regs.set(1+uint32(i)*2, v2)
			}
		}
	}
	return nil
}

// Convert from sparse representation to dense representation.
func (sk *Sketch) toNormal() {
	if len(sk.tmpSet) > 0 {
		sk.mergeSparse()
	}

	sk.regs = newRegisters(sk.m)
	for iter := sk.sparseList.Iter(); iter.HasNext(); {
		i, r := decodeHash(iter.Next(), sk.p, pp)
		sk.insert(i, r)
	}

	sk.sparse = false
	sk.tmpSet = nil
	sk.sparseList = nil
}

func (sk *Sketch) insert(i uint32, r uint8) {
	if r-sk.b >= capacity {
		//overflow
		db := sk.regs.min()
		if db > 0 {
			sk.b += db
			sk.regs.rebase(db)
		}
	}
	if r > sk.b {
		val := uint8(math.Min(float64(r-sk.b), float64(capacity-1)))
		if val > sk.regs.get(i) {
			sk.regs.set(i, uint8(val))
		}
	}
}

// Insert ...
func (sk *Sketch) Insert(e []byte) {
	x := sk.hash(e)
	if sk.sparse {
		sk.tmpSet.add(encodeHash(x, sk.p, pp))
		if uint32(len(sk.tmpSet))*100 > sk.m {
			sk.mergeSparse()
			if uint32(sk.sparseList.Len()) > sk.m {
				sk.toNormal()
			}
		}
	} else {
		i, r := getPosVal(x, sk.p)
		sk.insert(uint32(i), r)
	}
}

// Estimate ...
func (sk *Sketch) Estimate() uint64 {
	if sk.sparse {
		sk.mergeSparse()
		return uint64(linearCount(mp, mp-uint32(sk.sparseList.count)))
	}

	sum := float64(sk.regs.sum(sk.b))
	ez := float64(sk.regs.zeros())
	m := float64(sk.m)
	var est float64

	if sk.b == 0 {
		est = (sk.alpha * m * (m - ez) / (sum + beta(ez))) + 0.5
	} else {
		est = (sk.alpha * m * m / sum) + 0.5
	}

	return uint64(est + 0.5)
}

func (sk *Sketch) mergeSparse() {
	if len(sk.tmpSet) == 0 {
		return
	}

	keys := make(uint64Slice, 0, len(sk.tmpSet))
	for k := range sk.tmpSet {
		keys = append(keys, k)
	}
	sort.Sort(keys)

	newList := newCompressedList(int(sk.m))
	for iter, i := sk.sparseList.Iter(), 0; iter.HasNext() || i < len(keys); {
		if !iter.HasNext() {
			newList.Append(keys[i])
			i++
			continue
		}

		if i >= len(keys) {
			newList.Append(iter.Next())
			continue
		}

		x1, x2 := iter.Peek(), keys[i]
		if x1 == x2 {
			newList.Append(iter.Next())
			i++
		} else if x1 > x2 {
			newList.Append(x2)
			i++
		} else {
			newList.Append(iter.Next())
		}
	}

	sk.sparseList = newList
	sk.tmpSet = set{}
}

// MarshalBinary implements the encoding.BinaryMarshaler interface.
func (sk *Sketch) MarshalBinary() (data []byte, err error) {
	// Marshal a version marker.
	data = append(data, version)
	// Marshal p.
	data = append(data, byte(sk.p))
	// Marshal b
	data = append(data, byte(sk.b))

	if sk.sparse {
		// It's using the sparse representation.
		data = append(data, byte(1))

		// Add the tmp_set
		tsdata, err := sk.tmpSet.MarshalBinary()
		if err != nil {
			return nil, err
		}
		data = append(data, tsdata...)

		// Add the sparse representation
		sdata, err := sk.sparseList.MarshalBinary()
		if err != nil {
			return nil, err
		}
		return append(data, sdata...), nil
	}

	// It's using the dense representation.
	data = append(data, byte(0))

	// Add the dense sketch representation.
	sz := len(sk.regs.fields)
	data = append(data, []byte{
		byte(sz >> 24),
		byte(sz >> 16),
		byte(sz >> 8),
		byte(sz),
	}...)

	// Marshal each element in the list.
	for i := 0; i < len(sk.regs.fields); i++ {
		data = append(data, byte(sk.regs.fields[i]))
	}

	return data, nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface.
func (sk *Sketch) UnmarshalBinary(data []byte) error {
	// Unmarshal version. We may need this in the future if we make
	// non-compatible changes.
	_ = data[0]

	// Unmarshal p.
	p := uint8(data[1])

	newh, err := New(p)
	if err != nil {
		return err
	}
	*sk = *newh

	// Unmarshal b.
	sk.b = uint8(data[2])

	// h is now initialised with the correct p. We just need to fill the
	// rest of the details out.
	if data[3] == byte(1) {
		// Using the sparse representation.
		sk.sparse = true

		// Unmarshal the tmp_set.
		tssz := binary.BigEndian.Uint32(data[4:8])
		sk.tmpSet = make(map[uint32]struct{}, tssz)

		// We need to unmarshal tssz values in total, and each value requires us
		// to read 4 bytes.
		tsLastByte := int((tssz * 4) + 8)
		for i := 8; i < tsLastByte; i += 4 {
			k := binary.BigEndian.Uint32(data[i : i+4])
			sk.tmpSet[k] = struct{}{}
		}

		// Unmarshal the sparse representation.
		return sk.sparseList.UnmarshalBinary(data[tsLastByte:])
	}

	// Using the dense representation.
	sk.sparse = false
	sk.sparseList = nil
	sk.tmpSet = nil
	dsz := binary.BigEndian.Uint32(data[4:8])
	sk.regs = newRegisters(dsz * 2)
	data = data[8:]

	for i, val := range data {
		sk.regs.fields[i] = reg(val)
		if uint8(sk.regs.fields[i]<<4>>4) > 0 {
			sk.regs.nz--
		}
		if uint8(sk.regs.fields[i]>>4) > 0 {
			sk.regs.nz--
		}
	}

	return nil
}
