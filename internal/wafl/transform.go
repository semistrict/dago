// Package wafl instruments WebAssembly stores with passive page-dirty tracking.
//
// This is the stores-only, same-memory adaptation of semistrict/wafl. A small
// helper marks one byte per dirty page in a bitmap allocated by the host after
// instantiation. Tracking is disabled while the module starts, so the bitmap
// does not need to occupy a fixed address in the input binary.
package wafl

import (
	"bytes"
	"fmt"
	"io"

	wasmbinary "github.com/tetratelabs/wabin/binary"
	"github.com/tetratelabs/wabin/leb128"
	"github.com/tetratelabs/wabin/wasm"
)

const (
	BitmapBaseExport = "__wafl_bitmap_base"
	EnabledExport    = "__wafl_enabled"
	defaultPageSize  = uint32(4096)
)

// Config controls passive write tracking.
type Config struct {
	PageSize uint32
}

// Transform instruments scalar and bulk writes in wasm.
func Transform(source []byte, config Config) ([]byte, error) {
	if config.PageSize == 0 {
		config.PageSize = defaultPageSize
	}
	if !isPowerOfTwo(config.PageSize) || config.PageSize < 16 {
		return nil, fmt.Errorf("WAFL page size must be a power of two >= 16, got %d", config.PageSize)
	}
	module, err := wasmbinary.DecodeModule(source, wasm.CoreFeaturesV2)
	if err != nil {
		return nil, fmt.Errorf("decode WebAssembly: %w", err)
	}
	if module.MemorySection == nil {
		return nil, fmt.Errorf("WAFL requires one module-defined memory")
	}
	for _, item := range module.ImportSection {
		if item.Type == wasm.ExternTypeMemory {
			return nil, fmt.Errorf("WAFL does not support an imported main memory")
		}
	}

	globalBase := module.ImportGlobalCount() + uint32(len(module.GlobalSection))
	globalEnabled := globalBase + 1
	module.GlobalSection = append(module.GlobalSection,
		mutableI32Global(0), mutableI32Global(0),
	)
	module.ExportSection = append(module.ExportSection,
		&wasm.Export{Name: BitmapBaseExport, Type: wasm.ExternTypeGlobal, Index: globalBase},
		&wasm.Export{Name: EnabledExport, Type: wasm.ExternTypeGlobal, Index: globalEnabled},
	)

	helperType := uint32(len(module.TypeSection))
	module.TypeSection = append(module.TypeSection, &wasm.FunctionType{
		Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32},
	})
	helperIndex := module.ImportFuncCount() + uint32(len(module.FunctionSection))
	module.FunctionSection = append(module.FunctionSection, helperType)
	module.CodeSection = append(module.CodeSection, dirtyRangeHelper(config.PageSize, globalBase, globalEnabled))

	originalFunctions := len(module.CodeSection) - 1
	for index := 0; index < originalFunctions; index++ {
		typeIndex := module.FunctionSection[index]
		if int(typeIndex) >= len(module.TypeSection) {
			return nil, fmt.Errorf("function %d has invalid type index %d", index, typeIndex)
		}
		params := uint32(len(module.TypeSection[typeIndex].Params))
		if err := instrumentCode(module.CodeSection[index], params, helperIndex); err != nil {
			return nil, fmt.Errorf("instrument function %d: %w", index, err)
		}
	}
	return wasmbinary.EncodeModule(module), nil
}

func mutableI32Global(value int32) *wasm.Global {
	return &wasm.Global{
		Type: &wasm.GlobalType{ValType: wasm.ValueTypeI32, Mutable: true},
		Init: &wasm.ConstantExpression{Opcode: wasm.OpcodeI32Const, Data: leb128.EncodeInt32(value)},
	}
}

func dirtyRangeHelper(pageSize, bitmapBase, enabled uint32) *wasm.Code {
	pageShift := uint32(0)
	for 1<<pageShift < pageSize {
		pageShift++
	}
	// Params: address=0, length=1. Locals: page=2, lastPage=3.
	body := []byte{}
	body = opU32(body, wasm.OpcodeGlobalGet, enabled)
	body = append(body, wasm.OpcodeI32Eqz, wasm.OpcodeIf, 0x40, wasm.OpcodeReturn, wasm.OpcodeEnd)
	body = opU32(body, wasm.OpcodeLocalGet, 1)
	body = append(body, wasm.OpcodeI32Eqz, wasm.OpcodeIf, 0x40, wasm.OpcodeReturn, wasm.OpcodeEnd)
	body = opU32(body, wasm.OpcodeLocalGet, 0)
	body = append(body, wasm.OpcodeI32Const)
	body = append(body, leb128.EncodeInt32(int32(pageShift))...)
	body = append(body, wasm.OpcodeI32ShrU)
	body = opU32(body, wasm.OpcodeLocalSet, 2)
	body = opU32(body, wasm.OpcodeLocalGet, 0)
	body = opU32(body, wasm.OpcodeLocalGet, 1)
	body = append(body, wasm.OpcodeI32Add, wasm.OpcodeI32Const)
	body = append(body, leb128.EncodeInt32(1)...)
	body = append(body, wasm.OpcodeI32Sub, wasm.OpcodeI32Const)
	body = append(body, leb128.EncodeInt32(int32(pageShift))...)
	body = append(body, wasm.OpcodeI32ShrU)
	body = opU32(body, wasm.OpcodeLocalSet, 3)
	body = append(body, wasm.OpcodeBlock, 0x40, wasm.OpcodeLoop, 0x40)
	body = opU32(body, wasm.OpcodeGlobalGet, bitmapBase)
	body = opU32(body, wasm.OpcodeLocalGet, 2)
	body = append(body, wasm.OpcodeI32Add, wasm.OpcodeI32Const)
	body = append(body, leb128.EncodeInt32(1)...)
	body = append(body, wasm.OpcodeI32Store8)
	body = append(body, leb128.EncodeUint32(0)...)
	body = append(body, leb128.EncodeUint32(0)...)
	body = opU32(body, wasm.OpcodeLocalGet, 2)
	body = opU32(body, wasm.OpcodeLocalGet, 3)
	body = append(body, wasm.OpcodeI32Eq)
	body = opU32(body, wasm.OpcodeBrIf, 1)
	body = opU32(body, wasm.OpcodeLocalGet, 2)
	body = append(body, wasm.OpcodeI32Const)
	body = append(body, leb128.EncodeInt32(1)...)
	body = append(body, wasm.OpcodeI32Add)
	body = opU32(body, wasm.OpcodeLocalSet, 2)
	body = opU32(body, wasm.OpcodeBr, 0)
	body = append(body, wasm.OpcodeEnd, wasm.OpcodeEnd, wasm.OpcodeEnd)
	return &wasm.Code{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, Body: body}
}

type storeInfo struct {
	valueType wasm.ValueType
	width     uint32
	offset    uint32
}

type instruction struct {
	raw       []byte
	store     *storeInfo
	bulkWrite byte
}

const (
	bulkNone byte = iota
	bulkInit
	bulkCopy
	bulkFill
)

func instrumentCode(code *wasm.Code, params, helper uint32) error {
	instructions, err := decodeInstructions(code.Body)
	if err != nil {
		return err
	}
	needsStores := false
	for _, item := range instructions {
		if item.store != nil || item.bulkWrite != bulkNone {
			needsStores = true
			break
		}
	}
	if !needsStores {
		return nil
	}
	baseLocal := params + uint32(len(code.LocalTypes))
	addrLocal, auxLocal, lenLocal := baseLocal, baseLocal+1, baseLocal+2
	i32Local, i64Local, f32Local, f64Local := baseLocal+3, baseLocal+4, baseLocal+5, baseLocal+6
	code.LocalTypes = append(code.LocalTypes,
		wasm.ValueTypeI32, wasm.ValueTypeI32, wasm.ValueTypeI32,
		wasm.ValueTypeI32, wasm.ValueTypeI64, wasm.ValueTypeF32, wasm.ValueTypeF64,
	)
	result := make([]byte, 0, len(code.Body)+128)
	for _, item := range instructions {
		if item.store != nil {
			valueLocal := i32Local
			switch item.store.valueType {
			case wasm.ValueTypeI64:
				valueLocal = i64Local
			case wasm.ValueTypeF32:
				valueLocal = f32Local
			case wasm.ValueTypeF64:
				valueLocal = f64Local
			}
			result = opU32(result, wasm.OpcodeLocalSet, valueLocal)
			result = opU32(result, wasm.OpcodeLocalTee, addrLocal)
			if item.store.offset != 0 {
				result = append(result, wasm.OpcodeI32Const)
				result = append(result, leb128.EncodeInt32(int32(item.store.offset))...)
				result = append(result, wasm.OpcodeI32Add)
			}
			result = append(result, wasm.OpcodeI32Const)
			result = append(result, leb128.EncodeInt32(int32(item.store.width))...)
			result = opU32(result, wasm.OpcodeCall, helper)
			result = opU32(result, wasm.OpcodeLocalGet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, valueLocal)
			result = append(result, item.raw...)
			continue
		}
		switch item.bulkWrite {
		case bulkFill:
			result = opU32(result, wasm.OpcodeLocalSet, lenLocal)
			result = opU32(result, wasm.OpcodeLocalSet, auxLocal)
			result = opU32(result, wasm.OpcodeLocalSet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, lenLocal)
			result = opU32(result, wasm.OpcodeCall, helper)
			result = opU32(result, wasm.OpcodeLocalGet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, auxLocal)
			result = opU32(result, wasm.OpcodeLocalGet, lenLocal)
			result = append(result, item.raw...)
		case bulkCopy, bulkInit:
			result = opU32(result, wasm.OpcodeLocalSet, lenLocal)
			result = opU32(result, wasm.OpcodeLocalSet, auxLocal)
			result = opU32(result, wasm.OpcodeLocalSet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, lenLocal)
			result = opU32(result, wasm.OpcodeCall, helper)
			result = opU32(result, wasm.OpcodeLocalGet, addrLocal)
			result = opU32(result, wasm.OpcodeLocalGet, auxLocal)
			result = opU32(result, wasm.OpcodeLocalGet, lenLocal)
			result = append(result, item.raw...)
		default:
			result = append(result, item.raw...)
		}
	}
	code.Body = result
	return nil
}

func decodeInstructions(body []byte) ([]instruction, error) {
	reader := bytes.NewReader(body)
	result := []instruction{}
	for reader.Len() > 0 {
		start := len(body) - reader.Len()
		opcode, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		item := instruction{}
		switch opcode {
		case wasm.OpcodeBlock, wasm.OpcodeLoop, wasm.OpcodeIf:
			if _, _, err = leb128.DecodeInt33AsInt64(reader); err != nil {
				return nil, err
			}
		case wasm.OpcodeBr, wasm.OpcodeBrIf, wasm.OpcodeCall,
			wasm.OpcodeLocalGet, wasm.OpcodeLocalSet, wasm.OpcodeLocalTee,
			wasm.OpcodeGlobalGet, wasm.OpcodeGlobalSet, wasm.OpcodeTableGet,
			wasm.OpcodeTableSet, wasm.OpcodeRefFunc:
			_, _, err = leb128.DecodeUint32(reader)
		case wasm.OpcodeBrTable:
			var count uint32
			count, _, err = leb128.DecodeUint32(reader)
			for index := uint32(0); err == nil && index <= count; index++ {
				_, _, err = leb128.DecodeUint32(reader)
			}
		case wasm.OpcodeCallIndirect:
			_, _, err = leb128.DecodeUint32(reader)
			if err == nil {
				_, _, err = leb128.DecodeUint32(reader)
			}
		case wasm.OpcodeTypedSelect:
			var count uint32
			count, _, err = leb128.DecodeUint32(reader)
			if err == nil {
				err = skip(reader, int(count))
			}
		case wasm.OpcodeI32Load, wasm.OpcodeI64Load, wasm.OpcodeF32Load, wasm.OpcodeF64Load,
			wasm.OpcodeI32Load8S, wasm.OpcodeI32Load8U, wasm.OpcodeI32Load16S, wasm.OpcodeI32Load16U,
			wasm.OpcodeI64Load8S, wasm.OpcodeI64Load8U, wasm.OpcodeI64Load16S, wasm.OpcodeI64Load16U,
			wasm.OpcodeI64Load32S, wasm.OpcodeI64Load32U:
			_, _, err = leb128.DecodeUint32(reader)
			if err == nil {
				_, _, err = leb128.DecodeUint32(reader)
			}
		case wasm.OpcodeI32Store, wasm.OpcodeI64Store, wasm.OpcodeF32Store, wasm.OpcodeF64Store,
			wasm.OpcodeI32Store8, wasm.OpcodeI32Store16, wasm.OpcodeI64Store8,
			wasm.OpcodeI64Store16, wasm.OpcodeI64Store32:
			_, _, err = leb128.DecodeUint32(reader)
			var offset uint32
			if err == nil {
				offset, _, err = leb128.DecodeUint32(reader)
			}
			valueType, width := storeType(opcode)
			item.store = &storeInfo{valueType: valueType, width: width, offset: offset}
		case wasm.OpcodeMemorySize, wasm.OpcodeMemoryGrow:
			_, _, err = leb128.DecodeUint32(reader)
		case wasm.OpcodeI32Const:
			_, _, err = leb128.DecodeInt32(reader)
		case wasm.OpcodeI64Const:
			_, _, err = leb128.DecodeInt64(reader)
		case wasm.OpcodeF32Const:
			err = skip(reader, 4)
		case wasm.OpcodeF64Const:
			err = skip(reader, 8)
		case wasm.OpcodeRefNull:
			err = skip(reader, 1)
		case wasm.OpcodeMiscPrefix:
			var subopcode uint32
			subopcode, _, err = leb128.DecodeUint32(reader)
			if err == nil {
				err = decodeMisc(reader, subopcode, &item)
			}
		case wasm.OpcodeVecPrefix:
			return nil, fmt.Errorf("SIMD instruction prefix is not supported by the stores-only port")
		}
		if err != nil {
			return nil, fmt.Errorf("opcode 0x%x immediate: %w", opcode, err)
		}
		end := len(body) - reader.Len()
		item.raw = append([]byte(nil), body[start:end]...)
		result = append(result, item)
	}
	return result, nil
}

func decodeMisc(reader *bytes.Reader, opcode uint32, item *instruction) error {
	readIndex := func() error { _, _, err := leb128.DecodeUint32(reader); return err }
	switch opcode {
	case uint32(wasm.OpcodeMiscI32TruncSatF32S), uint32(wasm.OpcodeMiscI32TruncSatF32U),
		uint32(wasm.OpcodeMiscI32TruncSatF64S), uint32(wasm.OpcodeMiscI32TruncSatF64U),
		uint32(wasm.OpcodeMiscI64TruncSatF32S), uint32(wasm.OpcodeMiscI64TruncSatF32U),
		uint32(wasm.OpcodeMiscI64TruncSatF64S), uint32(wasm.OpcodeMiscI64TruncSatF64U):
		return nil
	case uint32(wasm.OpcodeMiscMemoryInit):
		item.bulkWrite = bulkInit
		if err := readIndex(); err != nil {
			return err
		}
		return readIndex()
	case uint32(wasm.OpcodeMiscMemoryCopy):
		item.bulkWrite = bulkCopy
		if err := readIndex(); err != nil {
			return err
		}
		return readIndex()
	case uint32(wasm.OpcodeMiscMemoryFill):
		item.bulkWrite = bulkFill
		return readIndex()
	case uint32(wasm.OpcodeMiscDataDrop), uint32(wasm.OpcodeMiscElemDrop),
		uint32(wasm.OpcodeMiscTableGrow), uint32(wasm.OpcodeMiscTableSize), uint32(wasm.OpcodeMiscTableFill):
		return readIndex()
	case uint32(wasm.OpcodeMiscTableInit), uint32(wasm.OpcodeMiscTableCopy):
		if err := readIndex(); err != nil {
			return err
		}
		return readIndex()
	default:
		return fmt.Errorf("unsupported miscellaneous opcode %d", opcode)
	}
}

func storeType(opcode byte) (wasm.ValueType, uint32) {
	switch opcode {
	case wasm.OpcodeI64Store:
		return wasm.ValueTypeI64, 8
	case wasm.OpcodeF32Store:
		return wasm.ValueTypeF32, 4
	case wasm.OpcodeF64Store:
		return wasm.ValueTypeF64, 8
	case wasm.OpcodeI32Store8:
		return wasm.ValueTypeI32, 1
	case wasm.OpcodeI32Store16:
		return wasm.ValueTypeI32, 2
	case wasm.OpcodeI64Store8:
		return wasm.ValueTypeI64, 1
	case wasm.OpcodeI64Store16:
		return wasm.ValueTypeI64, 2
	case wasm.OpcodeI64Store32:
		return wasm.ValueTypeI64, 4
	default:
		return wasm.ValueTypeI32, 4
	}
}

func opU32(target []byte, opcode byte, value uint32) []byte {
	target = append(target, opcode)
	return append(target, leb128.EncodeUint32(value)...)
}

func skip(reader *bytes.Reader, count int) error {
	if count < 0 || reader.Len() < count {
		return io.ErrUnexpectedEOF
	}
	_, err := reader.Seek(int64(count), io.SeekCurrent)
	return err
}

func isPowerOfTwo(value uint32) bool { return value != 0 && value&(value-1) == 0 }
