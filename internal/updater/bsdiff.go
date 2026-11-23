package updater

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"io"
)

// Binary patches follow Colin Percival's bsdiff format: a new file is encoded
// as control/diff/extra streams where code and resource shifts become mostly
// zero diff bytes compressed with independent DEFLATE streams.

var errDamagedPatch = errors.New("更新补丁已损坏")

// bsdiffDiff returns a compressed binary patch that transforms old into new.
func bsdiffDiff(old, new []byte) []byte {
	sa := suffixArray(old)
	type block struct {
		diff, extra int
		seek        int
	}
	var blocks []block
	var diff, extra []byte
	var scan, pos, length int
	var lastScan, lastPos, lastOffset int
	for scan < len(new) {
		oldScore := 0
		scan += length
		for scsc := scan; scan < len(new); scan++ {
			length, pos = suffixSearch(sa, old, new[scan:])
			for ; scsc < scan+length; scsc++ {
				if scsc+lastOffset < len(old) && old[scsc+lastOffset] == new[scsc] {
					oldScore++
				}
			}
			if length == oldScore && length != 0 || length > oldScore+8 {
				break
			}
			if scan+lastOffset < len(old) && old[scan+lastOffset] == new[scan] {
				oldScore--
			}
		}
		if length == oldScore && scan != len(new) {
			continue
		}

		lenF := 0
		for i, s, best := 0, 0, 0; lastScan+i < scan && lastPos+i < len(old); {
			if old[lastPos+i] == new[lastScan+i] {
				s++
			}
			i++
			if s*2-i > best*2-lenF {
				best, lenF = s, i
			}
		}
		lenB := 0
		if scan < len(new) {
			for i, s, best := 1, 0, 0; scan >= lastScan+i && pos >= i; i++ {
				if old[pos-i] == new[scan-i] {
					s++
				}
				if s*2-i > best*2-lenB {
					best, lenB = s, i
				}
			}
		}
		if overlap := lastScan + lenF - (scan - lenB); overlap > 0 {
			s, best, lenS := 0, 0, 0
			for i := range overlap {
				if new[lastScan+lenF-overlap+i] == old[lastPos+lenF-overlap+i] {
					s++
				}
				if new[scan-lenB+i] == old[pos-lenB+i] {
					s--
				}
				if s > best {
					best, lenS = s, i+1
				}
			}
			lenF += lenS - overlap
			lenB -= lenS
		}

		for i := range lenF {
			diff = append(diff, new[lastScan+i]-old[lastPos+i])
		}
		extra = append(extra, new[lastScan+lenF:scan-lenB]...)
		b := block{lenF, scan - lenB - (lastScan + lenF), pos - lenB - (lastPos + lenF)}
		if b.diff == 0 && b.extra == 0 && len(blocks) > 0 {
			blocks[len(blocks)-1].seek += b.seek
		} else {
			blocks = append(blocks, b)
		}
		lastScan, lastPos, lastOffset = scan-lenB, pos-lenB, pos-scan
	}
	var ctrl []byte
	for _, b := range blocks {
		ctrl = binary.AppendUvarint(ctrl, uint64(b.diff))
		ctrl = binary.AppendUvarint(ctrl, uint64(b.extra))
		ctrl = binary.AppendVarint(ctrl, int64(b.seek))
	}
	ctrl, diff, extra = deflateBytes(ctrl), deflateBytes(diff), deflateBytes(extra)
	patch := binary.AppendUvarint(nil, uint64(len(blocks)))
	patch = binary.AppendUvarint(patch, uint64(len(ctrl)))
	patch = binary.AppendUvarint(patch, uint64(len(diff)))
	return bytes.Join([][]byte{patch, ctrl, diff, extra}, nil)
}

const maxBsdiffMatch = 64 << 10

func suffixSearch(sa []int32, old, b []byte) (length, pos int) {
	b = b[:min(len(b), maxBsdiffMatch)]
	lo, hi := 0, len(sa)-1
	for hi-lo >= 2 {
		mid := lo + (hi-lo)/2
		if bytes.Compare(old[sa[mid]:], b) < 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	x, y := prefixMatchLen(old[sa[lo]:], b), prefixMatchLen(old[sa[hi]:], b)
	if x > y {
		return x, int(sa[lo])
	}
	return y, int(sa[hi])
}

func prefixMatchLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func deflateBytes(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

// bsdiffPatch streams the reconstructed file of size bytes to w from old
// (oldSize bytes) and patch (patchSize bytes).
func bsdiffPatch(w io.Writer, size int64, old io.ReaderAt, oldSize int64, patch io.ReaderAt, patchSize int64) error {
	var head [3 * binary.MaxVarintLen64]byte
	n, _ := patch.ReadAt(head[:min(patchSize, int64(len(head)))], 0)
	var blocks, ctrlLen, diffLen uint64
	start, ok := readUvarints(head[:n], &blocks, &ctrlLen, &diffLen)
	if !ok || size < 0 || blocks > uint64(size)+1 ||
		ctrlLen > uint64(patchSize-start) || diffLen > uint64(patchSize-start)-ctrlLen {
		return errDamagedPatch
	}
	extraStart := start + int64(ctrlLen) + int64(diffLen)
	ctrlR := flate.NewReader(io.NewSectionReader(patch, start, int64(ctrlLen)))
	defer ctrlR.Close()
	diffR := flate.NewReader(io.NewSectionReader(patch, start+int64(ctrlLen), int64(diffLen)))
	defer diffR.Close()
	extraR := flate.NewReader(io.NewSectionReader(patch, extraStart, patchSize-extraStart))
	defer extraR.Close()

	ctrl := bufio.NewReader(ctrlR)
	buf := make([]byte, 32<<10)
	oldBuf := make([]byte, len(buf))
	var oldPos, left int64 = 0, size
	for range blocks {
		d, err1 := binary.ReadUvarint(ctrl)
		e, err2 := binary.ReadUvarint(ctrl)
		seek, err3 := binary.ReadVarint(ctrl)
		if err1 != nil || err2 != nil || err3 != nil || d > uint64(left) || e > uint64(left)-d {
			return errDamagedPatch
		}
		left -= int64(d + e)
		for d > 0 {
			chunk := int(min(d, uint64(len(buf))))
			if _, err := io.ReadFull(diffR, buf[:chunk]); err != nil {
				return errDamagedPatch
			}
			clear(oldBuf[:chunk])
			if from, to := max(oldPos, 0), min(oldPos+int64(chunk), oldSize); from < to {
				if _, err := old.ReadAt(oldBuf[from-oldPos:to-oldPos], from); err != nil {
					return err
				}
			}
			for i := range chunk {
				buf[i] += oldBuf[i]
			}
			if _, err := w.Write(buf[:chunk]); err != nil {
				return err
			}
			oldPos += int64(chunk)
			d -= uint64(chunk)
		}
		if copied, err := io.CopyN(w, extraR, int64(e)); copied != int64(e) {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errDamagedPatch
			}
			return err
		}
		oldPos += seek
		if oldPos < -(1<<40) || oldPos > 1<<40 {
			return errDamagedPatch
		}
	}
	if left != 0 {
		return errDamagedPatch
	}
	return nil
}

func readUvarints(b []byte, vs ...*uint64) (int64, bool) {
	n := 0
	for _, v := range vs {
		x, k := binary.Uvarint(b[n:])
		if k <= 0 {
			return 0, false
		}
		*v, n = x, n+k
	}
	return int64(n), true
}

// suffixArray returns the suffix array of b (with empty suffix first) using
// Larsson and Sadakane's qsufsort algorithm.
func suffixArray(b []byte) []int32 {
	n := int32(len(b))
	I := make([]int32, n+1)
	V := make([]int32, n+1)
	var buckets [256]int32
	for _, c := range b {
		buckets[c]++
	}
	for i := 1; i < 256; i++ {
		buckets[i] += buckets[i-1]
	}
	for i := 255; i > 0; i-- {
		buckets[i] = buckets[i-1]
	}
	buckets[0] = 0
	for i, c := range b {
		buckets[c]++
		I[buckets[c]] = int32(i)
	}
	I[0] = n
	for i, c := range b {
		V[i] = buckets[c]
	}
	V[n] = 0
	for i := 1; i < 256; i++ {
		if buckets[i] == buckets[i-1]+1 {
			I[buckets[i]] = -1
		}
	}
	I[0] = -1

	for h := int32(1); I[0] != -(n + 1); h += h {
		var run int32
		i := int32(0)
		for i < n+1 {
			if I[i] < 0 {
				run -= I[i]
				i -= I[i]
				continue
			}
			if run != 0 {
				I[i-run] = -run
			}
			size := V[I[i]] + 1 - i
			splitSuffixGroup(I, V, i, size, h)
			i += size
			run = 0
		}
		if run != 0 {
			I[i-run] = -run
		}
	}
	for i := range n + 1 {
		I[V[i]] = i
	}
	return I
}

func splitSuffixGroup(I, V []int32, start, size, h int32) {
	for size >= 16 {
		x := V[I[start+size/2]+h]
		var jj, kk int32
		for i := start; i < start+size; i++ {
			switch v := V[I[i]+h]; {
			case v < x:
				jj++
			case v == x:
				kk++
			}
		}
		jj += start
		kk += jj
		i, j, k := start, int32(0), int32(0)
		for i < jj {
			switch v := V[I[i]+h]; {
			case v < x:
				i++
			case v == x:
				I[i], I[jj+j] = I[jj+j], I[i]
				j++
			default:
				I[i], I[kk+k] = I[kk+k], I[i]
				k++
			}
		}
		for jj+j < kk {
			if V[I[jj+j]+h] == x {
				j++
			} else {
				I[jj+j], I[kk+k] = I[kk+k], I[jj+j]
				k++
			}
		}
		if jj > start {
			splitSuffixGroup(I, V, start, jj-start, h)
		}
		for i := range kk - jj {
			V[I[jj+i]] = kk - 1
		}
		if jj == kk-1 {
			I[jj] = -1
		}
		size = start + size - kk
		start = kk
	}
	if size <= 0 {
		return
	}
	for k, j := start, int32(0); k < start+size; k += j {
		j = 1
		x := V[I[k]+h]
		for i := int32(1); k+i < start+size; i++ {
			v := V[I[k+i]+h]
			if v < x {
				x = v
				j = 0
			}
			if v == x {
				I[k+j], I[k+i] = I[k+i], I[k+j]
				j++
			}
		}
		for i := range j {
			V[I[k+i]] = k + j - 1
		}
		if j == 1 {
			I[k] = -1
		}
	}
}
