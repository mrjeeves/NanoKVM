//go:build linux

package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
)

const (
	isoSectorSize       = int64(2048)
	diskSectorSize      = int64(512)
	bootCatalogReadSize = int64(32 * 1024)
	bootImagePrimeMin   = int64(1024 * 1024)
	bootImagePrimeMax   = int64(32 * 1024 * 1024)
	espPrimeMax         = int64(32 * 1024 * 1024)
)

var efiSystemPartitionGUID = []byte{
	0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11,
	0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b,
}

// primeBootMedia makes every range firmware needs to discover the device local
// before the USB gadget is connected. The rest of the image remains lazy.
// Firmware has much shorter storage timeouts than an operating system, so a
// live mesh read while it walks El Torito or GPT commonly makes otherwise
// valid media disappear from the boot menu.
func (p *pagedImage) primeBootMedia(ctx context.Context, cdrom bool) error {
	if p.size <= 0 {
		return fmt.Errorf("remote media is empty")
	}

	// Cover MBR/GPT, ISO volume descriptors, and the backup GPT/footer.
	if err := p.primeRange(ctx, 0, 2*p.chunkSize); err != nil {
		return err
	}
	if err := p.primeRange(ctx, p.size-p.chunkSize, p.chunkSize); err != nil {
		return err
	}

	if cdrom {
		return p.primeElTorito(ctx)
	}
	return p.primeDiskBoot(ctx)
}

func (p *pagedImage) primeRange(ctx context.Context, offset, length int64) error {
	if length <= 0 || offset >= p.size {
		return nil
	}
	if offset < 0 {
		length += offset
		offset = 0
	}
	if length <= 0 {
		return nil
	}
	end := offset + length
	if end < offset || end > p.size {
		end = p.size
	}
	first := offset / p.chunkSize
	last := (end - 1) / p.chunkSize
	for index := first; index <= last; index++ {
		if _, err := p.getChunk(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

func (p *pagedImage) readRange(ctx context.Context, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 || offset >= p.size {
		return nil, nil
	}
	if length > p.size-offset {
		length = p.size - offset
	}
	data := make([]byte, int(length))
	n, err := p.readAt(ctx, data, offset)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}

func (p *pagedImage) primeElTorito(ctx context.Context) error {
	// ISO-9660 volume descriptors begin at sector 16. The El Torito boot
	// record points to a boot catalog that may live anywhere in the image.
	for sector := int64(16); sector < 80; sector++ {
		descriptor, err := p.readRange(ctx, sector*isoSectorSize, isoSectorSize)
		if err != nil {
			return err
		}
		if len(descriptor) < int(isoSectorSize) || !bytes.Equal(descriptor[1:6], []byte("CD001")) {
			continue
		}
		if descriptor[0] == 0xff {
			break
		}
		if descriptor[0] != 0 || !bytes.HasPrefix(descriptor[7:39], []byte("EL TORITO SPECIFICATION")) {
			continue
		}

		catalogLBA := int64(binary.LittleEndian.Uint32(descriptor[71:75]))
		catalogOffset := catalogLBA * isoSectorSize
		catalog, err := p.readRange(ctx, catalogOffset, bootCatalogReadSize)
		if err != nil {
			return err
		}
		return p.primeBootCatalog(ctx, catalog)
	}
	return nil
}

func (p *pagedImage) primeBootCatalog(ctx context.Context, catalog []byte) error {
	if len(catalog) < 64 || catalog[0] != 0x01 || catalog[30] != 0x55 || catalog[31] != 0xaa {
		return nil
	}

	remaining := bootImagePrimeMax * 2 // normally one legacy and one EFI image
	primeEntry := func(entry []byte) error {
		if len(entry) < 32 || entry[0] != 0x88 || remaining <= 0 {
			return nil
		}
		start := int64(binary.LittleEndian.Uint32(entry[8:12])) * isoSectorSize
		length := int64(binary.LittleEndian.Uint16(entry[6:8])) * diskSectorSize
		// Some no-emulation catalogs describe only the initial loader sectors.
		// Keeping the first MiB local covers the loader's immediate follow-up
		// reads without pretending the entire installer was uploaded.
		if length < bootImagePrimeMin {
			length = bootImagePrimeMin
		}
		if length > bootImagePrimeMax {
			length = bootImagePrimeMax
		}
		if length > remaining {
			length = remaining
		}
		if err := p.primeRange(ctx, start, length); err != nil {
			return err
		}
		remaining -= length
		return nil
	}

	if err := primeEntry(catalog[32:64]); err != nil {
		return err
	}
	for offset := 64; offset+32 <= len(catalog); {
		entry := catalog[offset : offset+32]
		switch entry[0] {
		case 0x90, 0x91:
			count := int(binary.LittleEndian.Uint16(entry[2:4]))
			offset += 32
			for i := 0; i < count && offset+32 <= len(catalog); i++ {
				if err := primeEntry(catalog[offset : offset+32]); err != nil {
					return err
				}
				offset += 32
			}
			if entry[0] == 0x91 {
				return nil
			}
		case 0x00:
			return nil
		default:
			offset += 32
		}
	}
	return nil
}

func (p *pagedImage) primeDiskBoot(ctx context.Context) error {
	header, err := p.readRange(ctx, diskSectorSize, diskSectorSize)
	if err != nil {
		return err
	}
	if len(header) >= int(diskSectorSize) && bytes.Equal(header[:8], []byte("EFI PART")) {
		entriesLBA := int64(binary.LittleEndian.Uint64(header[72:80]))
		entryCount := int64(binary.LittleEndian.Uint32(header[80:84]))
		entrySize := int64(binary.LittleEndian.Uint32(header[84:88]))
		if entryCount > 0 && entryCount <= 4096 && entrySize >= 128 && entrySize <= 4096 {
			entries, readErr := p.readRange(ctx, entriesLBA*diskSectorSize, entryCount*entrySize)
			if readErr != nil {
				return readErr
			}
			for offset := int64(0); offset+entrySize <= int64(len(entries)); offset += entrySize {
				entry := entries[offset : offset+entrySize]
				if !bytes.Equal(entry[:16], efiSystemPartitionGUID) {
					continue
				}
				first := int64(binary.LittleEndian.Uint64(entry[32:40]))
				last := int64(binary.LittleEndian.Uint64(entry[40:48]))
				if first > 0 && last >= first {
					length := (last - first + 1) * diskSectorSize
					if length > espPrimeMax {
						length = espPrimeMax
					}
					return p.primeRange(ctx, first*diskSectorSize, length)
				}
			}
		}
	}

	// Also support removable images using a legacy MBR EFI partition.
	mbr, err := p.readRange(ctx, 0, diskSectorSize)
	if err != nil || len(mbr) < int(diskSectorSize) {
		return err
	}
	for offset := 446; offset < 510; offset += 16 {
		entry := mbr[offset : offset+16]
		if entry[4] != 0xef {
			continue
		}
		first := int64(binary.LittleEndian.Uint32(entry[8:12]))
		sectors := int64(binary.LittleEndian.Uint32(entry[12:16]))
		length := sectors * diskSectorSize
		if length > espPrimeMax {
			length = espPrimeMax
		}
		return p.primeRange(ctx, first*diskSectorSize, length)
	}
	return nil
}
