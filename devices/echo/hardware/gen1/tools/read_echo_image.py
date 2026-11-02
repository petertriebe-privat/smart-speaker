"""Read selected ext3 startup files from the image without mounting or executing it."""
from pathlib import Path
import struct

import os
image = Path(os.environ['ECHO_IMAGE']).open('rb')
partition_offset = 32769 * 512

def read_at(offset, length):
    image.seek(partition_offset + offset)
    result = image.read(length)
    if len(result) != length:
        raise ValueError('Short image read')
    return result

def u32(buf, offset):
    return struct.unpack_from('<I', buf, offset)[0]

sb = read_at(1024, 1024)
assert struct.unpack_from('<H', sb, 56)[0] == 0xEF53
block_size = 1024 << u32(sb, 24)
inodes_per_group = u32(sb, 40)
inode_size = struct.unpack_from('<H', sb, 88)[0] or 128
descriptor_offset = (2 if block_size == 1024 else 1) * block_size

def inode(number):
    group, index = divmod(number - 1, inodes_per_group)
    descriptor = read_at(descriptor_offset + group * 32, 32)
    table_block = u32(descriptor, 8)
    return read_at(table_block * block_size + index * inode_size, inode_size)

def indirect(number, levels):
    if not number:
        return
    values = struct.unpack('<' + 'I' * (block_size // 4), read_at(number * block_size, block_size))
    for value in values:
        if not value:
            continue
        if levels == 1:
            yield value
        else:
            yield from indirect(value, levels - 1)

def content(number):
    node = inode(number)
    size = u32(node, 4)
    mode = struct.unpack_from('<H', node)[0]
    if mode & 0xF000 == 0xA000 and size <= 60:
        return node[40:40 + size]
    pointers = struct.unpack_from('<15I', node, 40)
    fanout = block_size // 4
    cache = {}
    def pointer_at(block, index):
        if not block:
            return 0
        if block not in cache:
            cache[block] = struct.unpack('<' + 'I' * fanout, read_at(block * block_size, block_size))
        return cache[block][index]
    def block_at(index):
        if index < 12:
            return pointers[index]
        index -= 12
        if index < fanout:
            return pointer_at(pointers[12], index)
        index -= fanout
        if index < fanout * fanout:
            return pointer_at(pointer_at(pointers[13], index // fanout), index % fanout)
        index -= fanout * fanout
        return pointer_at(pointer_at(pointer_at(pointers[14], index // (fanout * fanout)), (index // fanout) % fanout), index % fanout)
    chunks = []
    for i in range((size + block_size - 1) // block_size):
        b = block_at(i)
        chunks.append(read_at(b * block_size, block_size) if b else bytes(block_size))
    result = b''.join(chunks)[:size]
    assert len(result) == size
    return result

def directory(number):
    data = content(number)
    result = {}
    pos = 0
    while pos + 8 <= len(data):
        node, length, name_len, _ = struct.unpack_from('<IHBB', data, pos)
        if length < 8 or pos + length > len(data):
            raise ValueError('Invalid directory entry')
        if node:
            result[data[pos + 8:pos + 8 + name_len].decode('utf-8', 'replace')] = node
        pos += length
    return result

def resolve(path):
    number = 2
    for name in path.strip('/').split('/'):
        number = directory(number)[name]
    return number


def extract(path):
    import posixpath
    for _ in range(12):
        number = resolve(path)
        node = inode(number)
        data = content(number)
        if struct.unpack_from('<H', node)[0] & 0xF000 != 0xA000:
            return data
        dest = data.decode()
        path = dest if dest.startswith('/') else posixpath.normpath(posixpath.join(posixpath.dirname(path), dest))
    raise ValueError('Symlink loop')
