package auth

import (
	"fmt"
	"strconv"
	"strings"
)

// qrVersionSpec defines ISO/IEC 18004 ECC Level L parameters for QR Code Versions 1–10.
type qrVersionSpec struct {
	_ [0]func()

	version      int
	ecPerBlock   int
	group1Blocks int
	group1DataCW int
	group2Blocks int
	group2DataCW int
	alignCenters []int
}

var qrSpecsLevelL = []qrVersionSpec{
	{version: 1, ecPerBlock: 7, group1Blocks: 1, group1DataCW: 19},
	{version: 2, ecPerBlock: 10, group1Blocks: 1, group1DataCW: 34, alignCenters: []int{6, 18}},
	{version: 3, ecPerBlock: 15, group1Blocks: 1, group1DataCW: 55, alignCenters: []int{6, 22}},
	{version: 4, ecPerBlock: 20, group1Blocks: 1, group1DataCW: 80, alignCenters: []int{6, 26}},
	{version: 5, ecPerBlock: 26, group1Blocks: 1, group1DataCW: 108, alignCenters: []int{6, 30}},
	{version: 6, ecPerBlock: 18, group1Blocks: 2, group1DataCW: 68, alignCenters: []int{6, 34}},
	{version: 7, ecPerBlock: 20, group1Blocks: 2, group1DataCW: 78, alignCenters: []int{6, 22, 38}},
	{version: 8, ecPerBlock: 24, group1Blocks: 2, group1DataCW: 97, alignCenters: []int{6, 24, 42}},
	{version: 9, ecPerBlock: 30, group1Blocks: 2, group1DataCW: 116, alignCenters: []int{6, 26, 46}},
	{version: 10, ecPerBlock: 18, group1Blocks: 2, group1DataCW: 68, group2Blocks: 2, group2DataCW: 69, alignCenters: []int{6, 28, 50}},
}

// RenderQRCodeSVG encodes text (such as an otpauth://totp/... URI) into a pure-Go,
// zero-external-dependency ISO/IEC 18004 QR Code SVG with a 4-module quiet zone.
func RenderQRCodeSVG(text string, sizePx int) string {
	if sizePx <= 0 {
		sizePx = 220
	}
	matrix, ok := encodeQRMatrix([]byte(text))
	if !ok || len(matrix) == 0 {
		return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1" width="%d" height="%d"/>`, sizePx, sizePx)
	}

	const quietZone = 4
	n := len(matrix)
	viewSize := n + 2*quietZone

	var path strings.Builder
	path.Grow(n * n * 4)
	for r := 0; r < n; r++ {
		c := 0
		for c < n {
			if !matrix[r][c] {
				c++
				continue
			}
			run := 1
			for c+run < n && matrix[r][c+run] {
				run++
			}
			path.WriteByte('M')
			path.WriteString(strconv.Itoa(c + quietZone))
			path.WriteByte(',')
			path.WriteString(strconv.Itoa(r + quietZone))
			path.WriteByte('h')
			path.WriteString(strconv.Itoa(run))
			path.WriteString("v1h-")
			path.WriteString(strconv.Itoa(run))
			path.WriteByte('z')
			c += run
		}
	}

	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#ffffff"/><path fill="#000000" d="%s"/></svg>`,
		viewSize, viewSize, sizePx, sizePx, path.String(),
	)
}

func encodeQRMatrix(payload []byte) ([][]bool, bool) {
	var spec qrVersionSpec
	found := false
	for _, cand := range qrSpecsLevelL {
		totalDataCW := cand.group1Blocks*cand.group1DataCW + cand.group2Blocks*cand.group2DataCW
		countBits := 8
		if cand.version >= 10 {
			countBits = 16
		}
		// 4 mode bits + countBits <= totalDataCW*8
		maxBytes := (totalDataCW*8 - 4 - countBits) / 8
		if len(payload) <= maxBytes {
			spec = cand
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}

	dataCW := buildQRDataCodewords(payload, spec)
	finalCW := interleaveQRBlocks(dataCW, spec)

	size := 17 + 4*spec.version
	modules := make([][]bool, size)
	reserved := make([][]bool, size)
	for i := range modules {
		modules[i] = make([]bool, size)
		reserved[i] = make([]bool, size)
	}

	// 1. Finder patterns + 1-module white separators at 3 corners
	placeFinderPattern(modules, reserved, 0, 0)
	placeFinderPattern(modules, reserved, 0, size-7)
	placeFinderPattern(modules, reserved, size-7, 0)

	// 2. Alignment patterns (skipping any that overlap the 3 finder patterns)
	for _, r := range spec.alignCenters {
		for _, c := range spec.alignCenters {
			if (r == 6 && c == 6) || (r == 6 && c == size-7) || (r == size-7 && c == 6) {
				continue
			}
			placeAlignmentPattern(modules, reserved, r, c)
		}
	}

	// 3. Timing patterns along row 6 and col 6
	for i := 8; i < size-8; i++ {
		dark := i%2 == 0
		if !reserved[6][i] {
			modules[6][i] = dark
			reserved[6][i] = true
		}
		if !reserved[i][6] {
			modules[i][6] = dark
			reserved[i][6] = true
		}
	}

	// 4. Dark module + reserve Format Information modules
	modules[size-8][8] = true
	reserved[size-8][8] = true
	for i := 0; i < 9; i++ {
		reserved[8][i] = true
		reserved[i][8] = true
	}
	for i := 0; i < 8; i++ {
		reserved[8][size-1-i] = true
		reserved[size-1-i][8] = true
	}

	// 5. Version Information blocks for Version >= 7
	if spec.version >= 7 {
		placeVersionInfo(modules, reserved, spec.version, size)
	}

	// 6. Zigzag 2-column data + EC codeword placement (skipping vertical timing col 6)
	bitIdx := 0
	totalBits := len(finalCW) * 8
	upward := true
	for rightCol := size - 1; rightCol >= 1; rightCol -= 2 {
		if rightCol == 6 {
			rightCol = 5
		}
		for step := 0; step < size; step++ {
			r := step
			if upward {
				r = size - 1 - step
			}
			for dx := 0; dx < 2; dx++ {
				c := rightCol - dx
				if reserved[r][c] {
					continue
				}
				var bit bool
				if bitIdx < totalBits {
					cw := finalCW[bitIdx>>3]
					bit = ((cw >> (7 - (bitIdx & 7))) & 1) == 1
					bitIdx++
				}
				// Apply Mask Pattern 0: (row + col) % 2 == 0
				if (r+c)%2 == 0 {
					bit = !bit
				}
				modules[r][c] = bit
			}
		}
		upward = !upward
	}

	// 7. Format Information bits for ECC Level L (0b01) + Mask 0 (0b000)
	placeFormatInfo(modules, 0b01, 0, size)
	return modules, true
}

func buildQRDataCodewords(payload []byte, spec qrVersionSpec) []byte {
	totalDataCW := spec.group1Blocks*spec.group1DataCW + spec.group2Blocks*spec.group2DataCW
	capacityBits := totalDataCW * 8
	bits := make([]uint8, 0, capacityBits)

	appendBits := func(val int, width int) {
		for i := width - 1; i >= 0; i-- {
			bits = append(bits, uint8((val>>i)&1))
		}
	}

	// Mode indicator for Byte mode: 0100
	appendBits(0b0100, 4)
	countBits := 8
	if spec.version >= 10 {
		countBits = 16
	}
	appendBits(len(payload), countBits)
	for _, b := range payload {
		appendBits(int(b), 8)
	}

	// Terminator: up to 4 zero bits
	for i := 0; i < 4 && len(bits) < capacityBits; i++ {
		bits = append(bits, 0)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}

	out := make([]byte, 0, totalDataCW)
	for i := 0; i < len(bits); i += 8 {
		var cw byte
		for j := 0; j < 8; j++ {
			cw = (cw << 1) | bits[i+j]
		}
		out = append(out, cw)
	}

	padBytes := [2]byte{0xEC, 0x11}
	for p := 0; len(out) < totalDataCW; p++ {
		out = append(out, padBytes[p&1])
	}
	return out
}

func interleaveQRBlocks(dataCW []byte, spec qrVersionSpec) []byte {
	numBlocks := spec.group1Blocks + spec.group2Blocks
	dataBlocks := make([][]byte, numBlocks)
	ecBlocks := make([][]byte, numBlocks)
	gen := rsGeneratorPoly(spec.ecPerBlock)

	offset := 0
	maxDataLen := spec.group1DataCW
	if spec.group2DataCW > maxDataLen {
		maxDataLen = spec.group2DataCW
	}

	for b := 0; b < numBlocks; b++ {
		dLen := spec.group1DataCW
		if b >= spec.group1Blocks {
			dLen = spec.group2DataCW
		}
		blk := dataCW[offset : offset+dLen]
		offset += dLen
		dataBlocks[b] = blk
		ecBlocks[b] = rsComputeRemainder(blk, gen)
	}

	totalCW := offset + numBlocks*spec.ecPerBlock
	out := make([]byte, 0, totalCW)
	for i := 0; i < maxDataLen; i++ {
		for b := 0; b < numBlocks; b++ {
			if i < len(dataBlocks[b]) {
				out = append(out, dataBlocks[b][i])
			}
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for b := 0; b < numBlocks; b++ {
			out = append(out, ecBlocks[b][i])
		}
	}
	return out
}

// --- GF(256) Reed-Solomon arithmetic (primitive polynomial x^8 + x^4 + x^3 + x^2 + 1 = 0x11D) ---

func gf256Mul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x1D)
		z ^= ((y >> i) & 1) * x
	}
	return z
}

func rsGeneratorPoly(degree int) []byte {
	poly := make([]byte, degree)
	poly[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			poly[j] = gf256Mul(poly[j], root)
			if j+1 < degree {
				poly[j] ^= poly[j+1]
			}
		}
		root = gf256Mul(root, 0x02)
	}
	return poly
}

func rsComputeRemainder(data []byte, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for j, coeff := range gen {
			rem[j] ^= gf256Mul(coeff, factor)
		}
	}
	return rem
}

func placeFinderPattern(modules, reserved [][]bool, topRow, leftCol int) {
	size := len(modules)
	for dr := -1; dr <= 7; dr++ {
		r := topRow + dr
		if r < 0 || r >= size {
			continue
		}
		for dc := -1; dc <= 7; dc++ {
			c := leftCol + dc
			if c < 0 || c >= size {
				continue
			}
			inOuter := dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6
			inWhiteRing := dr >= 1 && dr <= 5 && dc >= 1 && dc <= 5
			inCore := dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4
			modules[r][c] = inOuter && (!inWhiteRing || inCore)
			reserved[r][c] = true
		}
	}
}

func placeAlignmentPattern(modules, reserved [][]bool, centerR, centerC int) {
	for dr := -2; dr <= 2; dr++ {
		for dc := -2; dc <= 2; dc++ {
			r, c := centerR+dr, centerC+dc
			dist := absInt(dr)
			if absInt(dc) > dist {
				dist = absInt(dc)
			}
			modules[r][c] = dist != 1
			reserved[r][c] = true
		}
	}
}

func placeFormatInfo(modules [][]bool, ecBits int, mask int, size int) {
	data := (ecBits << 3) | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := ((data << 10) | rem) ^ 0x5412

	// Copy 1: around top-left finder pattern (bit 0 = LSB at (0,8), bit 14 = MSB at (8,0))
	tlCoords := [15][2]int{
		{0, 8}, {1, 8}, {2, 8}, {3, 8}, {4, 8}, {5, 8}, {7, 8}, {8, 8},
		{8, 7}, {8, 5}, {8, 4}, {8, 3}, {8, 2}, {8, 1}, {8, 0},
	}
	for i := 0; i < 15; i++ {
		dark := ((bits >> i) & 1) == 1
		modules[tlCoords[i][0]][tlCoords[i][1]] = dark
	}

	// Copy 2: split across top-right (bits 0..7) and bottom-left (bits 8..14)
	for i := 0; i < 8; i++ {
		modules[8][size-1-i] = ((bits >> i) & 1) == 1
	}
	for i := 8; i < 15; i++ {
		modules[size-15+i][8] = ((bits >> i) & 1) == 1
	}
}

func placeVersionInfo(modules, reserved [][]bool, version int, size int) {
	rem := version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := (version << 12) | rem
	for i := 0; i < 18; i++ {
		dark := ((bits >> i) & 1) == 1
		a, b := i/3, size-11+(i%3)
		modules[a][b] = dark
		reserved[a][b] = true
		modules[b][a] = dark
		reserved[b][a] = true
	}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
