// HCI transport over MediaTek's /dev/stpbt: H4 packets (type byte + packet) written and read as
// they are. Opening the device turns the chip's Bluetooth on through WMT, closing turns it off.
// A read may hold several packets or end inside one; the rest waits for the next read.

#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "btstack_debug.h"
#include "btstack_run_loop.h"
#include "btstack_util.h"
#include "hci.h"
#include "hci_transport.h"

#include "hci_transport_stpbt.h"

static const char *device = "/dev/stpbt";
static btstack_data_source_t source;
static void (*handler)(uint8_t packet_type, uint8_t *packet, uint16_t size);

static uint8_t in[2 * (1 + HCI_INCOMING_PACKET_BUFFER_SIZE)];
static size_t in_len;
static uint8_t out[1 + HCI_OUTGOING_PACKET_BUFFER_SIZE];

// packet_length: the whole H4 packet's length once its header is in, 0 while it is not.
static size_t packet_length(const uint8_t *p, size_t n) {
    switch (p[0]) {
    case HCI_EVENT_PACKET:
        return n >= 3 ? 3 + p[2] : 0;
    case HCI_ACL_DATA_PACKET:
        return n >= 5 ? 5 + little_endian_read_16(p, 3) : 0;
    case HCI_SCO_DATA_PACKET:
        return n >= 4 ? 4 + p[3] : 0;
    default:
        return (size_t)-1;
    }
}

static void process(btstack_data_source_t *ds, btstack_data_source_callback_type_t type) {
    (void)type;
    ssize_t n = read(ds->source.fd, in + in_len, sizeof(in) - in_len);
    if (n <= 0) {
        if (n < 0 && errno != EAGAIN && errno != EINTR) log_error("stpbt: read: %s", strerror(errno));
        return;
    }
    in_len += (size_t)n;
    size_t pos = 0;
    while (pos < in_len) {
        size_t len = packet_length(in + pos, in_len - pos);
        if (len == (size_t)-1) {
            log_error("stpbt: unknown packet type 0x%02x, %u bytes dropped", in[pos], (unsigned)(in_len - pos));
            pos = in_len;
            break;
        }
        if (len == 0 || pos + len > in_len) break;
        if (in[pos] != HCI_SCO_DATA_PACKET) handler(in[pos], in + pos + 1, (uint16_t)(len - 1));
        pos += len;
    }
    memmove(in, in + pos, in_len - pos);
    in_len -= pos;
}

static int transport_open(void) {
    int fd = open(device, O_RDWR | O_NONBLOCK);
    if (fd < 0) {
        log_error("stpbt: open %s: %s", device, strerror(errno));
        return -1;
    }
    in_len = 0;
    btstack_run_loop_set_data_source_fd(&source, fd);
    btstack_run_loop_set_data_source_handler(&source, &process);
    btstack_run_loop_enable_data_source_callbacks(&source, DATA_SOURCE_CALLBACK_READ);
    btstack_run_loop_add_data_source(&source);
    return 0;
}

static int transport_close(void) {
    btstack_run_loop_remove_data_source(&source);
    close(source.source.fd);
    source.source.fd = -1;
    return 0;
}

static int transport_send(uint8_t packet_type, uint8_t *packet, int size) {
    out[0] = packet_type;
    memcpy(out + 1, packet, (size_t)size);
    size_t done = 0, total = (size_t)size + 1;
    while (done < total) {
        ssize_t n = write(source.source.fd, out + done, total - done);
        if (n < 0) {
            if (errno == EAGAIN || errno == EINTR) {
                usleep(1000);
                continue;
            }
            log_error("stpbt: write: %s", strerror(errno));
            return -1;
        }
        done += (size_t)n;
    }
    return 0;
}

static void transport_register(void (*h)(uint8_t packet_type, uint8_t *packet, uint16_t size)) {
    handler = h;
}

static const hci_transport_t transport = {
    .name = "stpbt",
    .open = transport_open,
    .close = transport_close,
    .register_packet_handler = transport_register,
    .send_packet = transport_send,
};

const hci_transport_t *hci_transport_stpbt_instance(void) {
    return &transport;
}

// MediaTek's vendor command to set the public address (as Linux's btmtk: opcode 0xfc1a).
static void set_bd_addr_command(bd_addr_t addr, uint8_t *cmd) {
    cmd[0] = 0x1a;
    cmd[1] = 0xfc;
    cmd[2] = 6;
    reverse_bd_addr(addr, &cmd[3]);
}

static const btstack_chipset_t chipset = {
    .name = "MediaTek combo",
    .set_bd_addr_command = set_bd_addr_command,
};

const btstack_chipset_t *btstack_chipset_stpbt_instance(void) {
    return &chipset;
}
