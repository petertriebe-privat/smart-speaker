// btspeaker: the Echo Dot 2 as Bluetooth A2DP source (#74), BTstack on MediaTek's /dev/stpbt.
//
//   btspeaker scan                  inquiry for ~12 s: "DEVICE <addr> cod=<cod> rssi=<dBm> name=<name>"
//   btspeaker tone <addr> [seconds] connects, plays 440 Hz at -12 dBFS, disconnects
//   btspeaker play <addr>           streams s16le stereo PCM from stdin at the negotiated rate
//                                   (printed as "STREAMING rate=<hz>"); silence while stdin is dry.
//                                   Runs until stdin closes: connectable, so the speaker may connect
//                                   itself, and connecting out again every 15 s.
//
// Options before the command: -d <dir> for the link keys (default /data/assistant/bt), -b <addr> for
// the Dot's own address (default: the WiFi MAC + 1, as the chip comes up with a placeholder),
// -n <name> for the name the speaker lists (default "Jarvis"). In play mode fd 3, when open,
// takes commands: "VOLUME <0..100>" sets the speaker's volume (AVRCP absolute volume, with -v).
// State lines on stdout (READY, CONNECTED, STREAMING, DELAY, DISCONNECTED, ERROR) are for the assistant.

#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <math.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "btstack.h"
#include "btstack_run_loop_posix.h"
#include "btstack_signal.h"
#include "btstack_tlv_posix.h"
#include "classic/btstack_link_key_db_tlv.h"
#include "hci_dump.h"
#include "hci_dump_posix_stdout.h"

#include "hci_transport_stpbt.h"

#define AUDIO_TICK_MS 10
#define SBC_STORAGE_SIZE 1030
#define PREFERRED_RATE 48000
#define INQUIRY_1280MS 9 // 11.5 s
#define CONNECT_TIMEOUT_MS 30000
#define RETRY_MS 15000      // play: connecting out again after a failure (inside a pairing window)
#define RECONNECT_MS 3000   // play: after a link that worked dropped
#define FIFO_BYTES (48000 * 4 / 2) // 0.5 s of stereo s16

enum mode { MODE_SCAN, MODE_TONE, MODE_PLAY };

static enum mode mode;
static bd_addr_t remote;
static int tone_seconds = 5;
static const char *key_dir = "/data/assistant/bt";
static bd_addr_t own_addr;
static bool own_addr_set;
static const char *local_name = "Jarvis";

static btstack_packet_callback_registration_t hci_registration;
static btstack_tlv_posix_t tlv_context;
static char tlv_path[160];

static uint8_t sdp_a2dp_source_record[150];
static uint8_t sdp_avrcp_target_record[200];
static uint8_t sdp_avrcp_controller_record[200];
static uint8_t sdp_device_id_record[100];
static uint16_t avrcp_cid;
static int volume = -1; // 0..100 from fd 3 (VOLUME n), -1 = the speaker keeps its own
static btstack_data_source_t control_source;
static char control_line[64];
static size_t control_len;
static uint8_t sbc_capabilities[] = {
    (AVDTP_SBC_44100 << 4) | (AVDTP_SBC_48000 << 4) | AVDTP_SBC_STEREO | AVDTP_SBC_JOINT_STEREO,
    0xFF, 2, 53};
static uint8_t sbc_configuration[4];

static const btstack_sbc_encoder_t *encoder;
static btstack_sbc_encoder_bluedroid_t encoder_state;

static struct {
    uint16_t a2dp_cid;
    uint8_t local_seid;
    int rate;
    int block_length, subbands, min_bitpool, max_bitpool;
    btstack_sbc_channel_mode_t channel_mode;
    btstack_sbc_allocation_method_t allocation;
    bool streaming;
    btstack_timer_source_t tick;
    uint32_t last_tick_ms;
    uint32_t missed; // sample remainder (x1000)
    uint32_t samples_ready;
    uint32_t rtp_timestamp;
    int max_payload;
    uint8_t storage[SBC_STORAGE_SIZE];
    uint16_t storage_count;
    bool ready_to_send;
    uint32_t started_ms;
    double phase;
} m;

static btstack_timer_source_t deadline;
static btstack_timer_source_t retry;
static int exit_code;
static bool shutting_down;

// stdin PCM (play mode)
static btstack_data_source_t stdin_source;
static uint8_t fifo[FIFO_BYTES];
static size_t fifo_head, fifo_len;
static uint32_t underruns;

static void shutdown_with(int code) {
    if (shutting_down) return;
    shutting_down = true;
    exit_code = code;
    if (m.a2dp_cid) {
        a2dp_source_disconnect(m.a2dp_cid);
        return; // HCI goes off once the signaling is released
    }
    hci_power_control(HCI_POWER_OFF);
}

static void on_deadline(btstack_timer_source_t *t) {
    (void)t;
    if (mode == MODE_SCAN) {
        shutdown_with(0);
        return;
    }
    printf("ERROR timeout\n");
    shutdown_with(2);
}

static void arm_deadline(uint32_t ms) {
    btstack_run_loop_remove_timer(&deadline);
    btstack_run_loop_set_timer_handler(&deadline, on_deadline);
    btstack_run_loop_set_timer(&deadline, ms);
    btstack_run_loop_add_timer(&deadline);
}

static void connect_out(void) {
    if (shutting_down || m.a2dp_cid) return;
    uint8_t status = a2dp_source_establish_stream(remote, &m.a2dp_cid);
    if (status != ERROR_CODE_SUCCESS) printf("ERROR establish status=0x%02x\n", status);
}

static void on_retry(btstack_timer_source_t *t) {
    (void)t;
    connect_out();
}

// connect_later: play mode's answer to a failed or dropped link (the others end).
static void connect_later(uint32_t ms) {
    btstack_run_loop_remove_timer(&retry);
    btstack_run_loop_set_timer_handler(&retry, on_retry);
    btstack_run_loop_set_timer(&retry, ms);
    btstack_run_loop_add_timer(&retry);
}

// --- audio ---

// stdin_ready: PCM into the FIFO; when it is full the oldest goes, so a stalled link never builds
// up delay (and the assistant is never blocked by it).
static void stdin_ready(btstack_data_source_t *ds, btstack_data_source_callback_type_t type) {
    (void)type;
    uint8_t buf[4096];
    for (;;) {
        ssize_t n = read(ds->source.fd, buf, sizeof(buf));
        if (n == 0) { // the writer closed stdin: done
            btstack_run_loop_remove_data_source(ds);
            shutdown_with(0);
            return;
        }
        if (n < 0) return; // EAGAIN
        for (ssize_t i = 0; i < n;) {
            if (fifo_len == FIFO_BYTES) {
                fifo_head = (fifo_head + 4) % FIFO_BYTES;
                fifo_len -= 4;
            }
            size_t tail = (fifo_head + fifo_len) % FIFO_BYTES;
            size_t chunk = (size_t)(n - i);
            if (chunk > FIFO_BYTES - fifo_len) chunk = FIFO_BYTES - fifo_len;
            if (chunk > FIFO_BYTES - tail) chunk = FIFO_BYTES - tail;
            memcpy(fifo + tail, buf + i, chunk);
            fifo_len += chunk;
            i += (ssize_t)chunk;
        }
    }
}

static void produce(int16_t *pcm, int frames) {
    if (mode == MODE_TONE) {
        for (int i = 0; i < frames; i++) {
            int16_t v = (int16_t)(8192 * sin(m.phase));
            m.phase += 2 * M_PI * 440 / m.rate;
            if (m.phase > 2 * M_PI) m.phase -= 2 * M_PI;
            pcm[2 * i] = pcm[2 * i + 1] = v;
        }
        return;
    }
    size_t want = (size_t)frames * 4, got = 0;
    uint8_t *dst = (uint8_t *)pcm;
    while (got < want && fifo_len >= 4) {
        size_t chunk = want - got;
        if (chunk > fifo_len) chunk = fifo_len & ~(size_t)3;
        if (fifo_head + chunk > FIFO_BYTES) chunk = FIFO_BYTES - fifo_head;
        memcpy(dst + got, fifo + fifo_head, chunk);
        fifo_head = (fifo_head + chunk) % FIFO_BYTES;
        fifo_len -= chunk;
        got += chunk;
    }
    if (got < want) {
        if (got > 0 || fifo_len == 0) underruns++;
        memset(dst + got, 0, want - got);
    }
}

static void fill_sbc(void) {
    unsigned frames = encoder->num_audio_frames(&encoder_state);
    uint16_t len = encoder->sbc_buffer_length(&encoder_state);
    while (m.samples_ready >= frames && (m.max_payload - m.storage_count) >= len) {
        int16_t pcm[256 * 2];
        produce(pcm, (int)frames);
        encoder->encode_signed_16(&encoder_state, pcm, &m.storage[1 + m.storage_count]);
        m.storage_count += encoder->sbc_buffer_length(&encoder_state);
        m.samples_ready -= frames;
    }
}

static void send_media(void) {
    int frame_len = encoder->sbc_buffer_length(&encoder_state);
    uint8_t frames = (uint8_t)(m.storage_count / frame_len);
    m.storage[0] = frames;
    a2dp_source_stream_send_media_payload_rtp(m.a2dp_cid, m.local_seid, 0, m.rtp_timestamp, m.storage, m.storage_count + 1);
    m.rtp_timestamp += frames * encoder->num_audio_frames(&encoder_state);
    m.storage_count = 0;
    m.ready_to_send = false;
}

static void on_tick(btstack_timer_source_t *t) {
    btstack_run_loop_set_timer(t, AUDIO_TICK_MS);
    btstack_run_loop_add_timer(t);
    uint32_t now = btstack_run_loop_get_time_ms();
    uint32_t period = m.last_tick_ms ? now - m.last_tick_ms : AUDIO_TICK_MS;
    m.last_tick_ms = now;
    m.samples_ready += period * m.rate / 1000;
    m.missed += (period * m.rate) % 1000;
    while (m.missed >= 1000) {
        m.samples_ready++;
        m.missed -= 1000;
    }
    if (mode == MODE_TONE && now - m.started_ms > (uint32_t)tone_seconds * 1000) {
        btstack_run_loop_remove_timer(t);
        m.streaming = false;
        printf("TONE done\n");
        shutdown_with(0);
        return;
    }
    if (m.ready_to_send) return;
    fill_sbc();
    if (m.storage_count + encoder->sbc_buffer_length(&encoder_state) > m.max_payload) {
        m.ready_to_send = true;
        a2dp_source_stream_endpoint_request_can_send_now(m.a2dp_cid, m.local_seid);
    }
}

static void start_audio(void) {
    m.max_payload = btstack_min(a2dp_max_media_payload_size(m.a2dp_cid, m.local_seid), SBC_STORAGE_SIZE);
    m.storage_count = 0;
    m.ready_to_send = false;
    m.samples_ready = 0;
    m.missed = 0;
    m.last_tick_ms = 0;
    m.started_ms = btstack_run_loop_get_time_ms();
    m.streaming = true;
    btstack_run_loop_remove_timer(&m.tick);
    btstack_run_loop_set_timer_handler(&m.tick, on_tick);
    btstack_run_loop_set_timer(&m.tick, AUDIO_TICK_MS);
    btstack_run_loop_add_timer(&m.tick);
}

static void stop_audio(void) {
    m.streaming = false;
    btstack_run_loop_remove_timer(&m.tick);
}

// --- trace ---

// While <keydir>/trace exists, HCI commands, events and the small ACL packets (L2CAP signaling,
// AVDTP, AVRCP; not the audio) go to /tmp/btspeaker.trace: what the speaker did before it
// dropped the link.
static FILE *trace;

static void trace_packet(uint8_t packet_type, uint8_t in, uint8_t *packet, uint16_t len) {
    if (packet_type == HCI_ACL_DATA_PACKET && len > 200) return;
    if (packet_type == HCI_EVENT_PACKET && packet[0] == HCI_EVENT_NUMBER_OF_COMPLETED_PACKETS) return;
    uint32_t ms = btstack_run_loop_get_time_ms();
    const char *kind = packet_type == HCI_COMMAND_DATA_PACKET ? "CMD" : packet_type == HCI_EVENT_PACKET ? "EVT" : "ACL";
    fprintf(trace, "%u.%03u %s %s", ms / 1000, ms % 1000, kind, in ? "<=" : "=>");
    for (uint16_t i = 0; i < len; i++) fprintf(trace, " %02x", packet[i]);
    fputc('\n', trace);
    fflush(trace);
}

static void trace_message(int level, const char *format, va_list args) {
    (void)level;
    vfprintf(trace, format, args);
    fputc('\n', trace);
    fflush(trace);
}

static const hci_dump_t trace_dump = {.log_packet = trace_packet, .log_message = trace_message};

// --- volume ---

static btstack_timer_source_t volume_timer;

// send_volume: the volume to the speaker as AVRCP absolute volume (0..127), once AVRCP is up;
// only with -v: an Alexa Echo answered neither the volume notification nor the absolute volume
// and dropped the link ~15 s later every time (20.10.2026).
static bool absolute_volume;

static void send_volume(void) {
    if (!absolute_volume || !avrcp_cid || volume < 0) return;
    uint8_t status = avrcp_controller_set_absolute_volume(avrcp_cid, (uint8_t)(volume * 127 / 100));
    printf("VOLUME sent %d status=0x%02x\n", volume, status);
}

static void on_volume_timer(btstack_timer_source_t *t) {
    (void)t;
    send_volume();
    fflush(stdout);
}

// control_ready: commands from the assistant on fd 3, one per line ("VOLUME 40").
static void control_ready(btstack_data_source_t *ds, btstack_data_source_callback_type_t type) {
    (void)type;
    ssize_t n = read(ds->source.fd, control_line + control_len, sizeof(control_line) - 1 - control_len);
    if (n <= 0) {
        if (n == 0) btstack_run_loop_remove_data_source(ds);
        return;
    }
    control_len += (size_t)n;
    control_line[control_len] = 0;
    char *nl;
    while ((nl = strchr(control_line, '\n'))) {
        *nl = 0;
        int v;
        if (sscanf(control_line, "VOLUME %d", &v) == 1 && v >= 0 && v <= 100) {
            volume = v;
            send_volume();
        }
        size_t rest = control_len - (size_t)(nl + 1 - control_line);
        memmove(control_line, nl + 1, rest + 1);
        control_len = rest;
    }
    if (control_len == sizeof(control_line) - 1) control_len = 0; // a line too long: dropped
    fflush(stdout);
}

// --- events ---

// AVRCP: an Alexa Echo drops a source that offers A2DP alone after ~18 s and refuses it after
// (20.10.2026); phones bring AVRCP, so the Dot does too: as target (play status, play/pause from
// the speaker) and as controller (the speaker's volume).
static void avrcp_handler(uint8_t packet_type, uint16_t channel, uint8_t *packet, uint16_t size) {
    (void)channel;
    (void)size;
    if (packet_type != HCI_EVENT_PACKET || hci_event_packet_get_type(packet) != HCI_EVENT_AVRCP_META) return;
    switch (packet[2]) {
    case AVRCP_SUBEVENT_CONNECTION_ESTABLISHED: {
        uint8_t status = avrcp_subevent_connection_established_get_status(packet);
        if (status != ERROR_CODE_SUCCESS) {
            printf("AVRCP connect status=0x%02x\n", status);
            break;
        }
        avrcp_cid = avrcp_subevent_connection_established_get_avrcp_cid(packet);
        printf("AVRCP connected\n");
        avrcp_target_support_event(avrcp_cid, AVRCP_NOTIFICATION_EVENT_PLAYBACK_STATUS_CHANGED);
        avrcp_target_set_playback_status(avrcp_cid, m.streaming ? AVRCP_PLAYBACK_STATUS_PLAYING : AVRCP_PLAYBACK_STATUS_STOPPED);
        if (absolute_volume) avrcp_controller_enable_notification(avrcp_cid, AVRCP_NOTIFICATION_EVENT_VOLUME_CHANGED);
        // at once it was refused (0x0c: AVRCP still busy setting up, 20.10.2026)
        btstack_run_loop_remove_timer(&volume_timer);
        btstack_run_loop_set_timer_handler(&volume_timer, on_volume_timer);
        btstack_run_loop_set_timer(&volume_timer, 2000);
        btstack_run_loop_add_timer(&volume_timer);
        break;
    }
    case AVRCP_SUBEVENT_CONNECTION_RELEASED:
        printf("AVRCP released\n");
        avrcp_cid = 0;
        break;
    default:
        break;
    }
    fflush(stdout);
}

static void avrcp_target_handler(uint8_t packet_type, uint16_t channel, uint8_t *packet, uint16_t size) {
    (void)channel;
    (void)size;
    if (packet_type != HCI_EVENT_PACKET || hci_event_packet_get_type(packet) != HCI_EVENT_AVRCP_META) return;
    switch (packet[2]) {
    case AVRCP_SUBEVENT_PLAY_STATUS_QUERY:
        avrcp_target_play_status(avrcp_cid, 0xFFFFFFFF, 0xFFFFFFFF,
                                 m.streaming ? AVRCP_PLAYBACK_STATUS_PLAYING : AVRCP_PLAYBACK_STATUS_STOPPED);
        break;
    case AVRCP_SUBEVENT_OPERATION:
        if (avrcp_subevent_operation_get_button_pressed(packet))
            printf("AVRCP operation %s\n", avrcp_operation2str(avrcp_subevent_operation_get_operation_id(packet)));
        break;
    default:
        break;
    }
    fflush(stdout);
}

static void avrcp_controller_handler(uint8_t packet_type, uint16_t channel, uint8_t *packet, uint16_t size) {
    (void)channel;
    (void)size;
    if (packet_type != HCI_EVENT_PACKET || hci_event_packet_get_type(packet) != HCI_EVENT_AVRCP_META) return;
    if (packet[2] == AVRCP_SUBEVENT_NOTIFICATION_VOLUME_CHANGED) {
        printf("VOLUME %d\n", avrcp_subevent_notification_volume_changed_get_absolute_volume(packet) * 100 / 127);
        fflush(stdout);
    }
}

static void a2dp_handler(uint8_t packet_type, uint16_t channel, uint8_t *packet, uint16_t size) {
    (void)channel;
    (void)size;
    if (packet_type != HCI_EVENT_PACKET || hci_event_packet_get_type(packet) != HCI_EVENT_A2DP_META) return;
    uint8_t status;
    switch (hci_event_a2dp_meta_get_subevent_code(packet)) {
    case A2DP_SUBEVENT_SIGNALING_CONNECTION_ESTABLISHED:
        status = a2dp_subevent_signaling_connection_established_get_status(packet);
        if (status != ERROR_CODE_SUCCESS) {
            printf("ERROR connect status=0x%02x\n", status);
            m.a2dp_cid = 0;
            if (mode == MODE_PLAY) connect_later(RETRY_MS);
            else shutdown_with(3);
            break;
        }
        btstack_run_loop_remove_timer(&retry);
        m.a2dp_cid = a2dp_subevent_signaling_connection_established_get_a2dp_cid(packet);
        bd_addr_t peer;
        a2dp_subevent_signaling_connection_established_get_bd_addr(packet, peer);
        printf("CONNECTED %s\n", bd_addr_to_str(peer));
        break;
    case A2DP_SUBEVENT_SIGNALING_MEDIA_CODEC_SBC_CONFIGURATION: {
        m.rate = a2dp_subevent_signaling_media_codec_sbc_configuration_get_sampling_frequency(packet);
        m.block_length = a2dp_subevent_signaling_media_codec_sbc_configuration_get_block_length(packet);
        m.subbands = a2dp_subevent_signaling_media_codec_sbc_configuration_get_subbands(packet);
        m.min_bitpool = a2dp_subevent_signaling_media_codec_sbc_configuration_get_min_bitpool_value(packet);
        m.max_bitpool = a2dp_subevent_signaling_media_codec_sbc_configuration_get_max_bitpool_value(packet);
        m.allocation = (btstack_sbc_allocation_method_t)(a2dp_subevent_signaling_media_codec_sbc_configuration_get_allocation_method(packet) - 1);
        switch ((avdtp_channel_mode_t)a2dp_subevent_signaling_media_codec_sbc_configuration_get_channel_mode(packet)) {
        case AVDTP_CHANNEL_MODE_JOINT_STEREO: m.channel_mode = SBC_CHANNEL_MODE_JOINT_STEREO; break;
        case AVDTP_CHANNEL_MODE_STEREO: m.channel_mode = SBC_CHANNEL_MODE_STEREO; break;
        case AVDTP_CHANNEL_MODE_DUAL_CHANNEL: m.channel_mode = SBC_CHANNEL_MODE_DUAL_CHANNEL; break;
        default: m.channel_mode = SBC_CHANNEL_MODE_MONO; break;
        }
        printf("CODEC sbc rate=%d blocks=%d subbands=%d bitpool=%d..%d mode=%d\n", m.rate, m.block_length,
               m.subbands, m.min_bitpool, m.max_bitpool, m.channel_mode);
        encoder = btstack_sbc_encoder_bluedroid_init_instance(&encoder_state);
        encoder->configure(&encoder_state, SBC_MODE_STANDARD, m.block_length, m.subbands, m.allocation, m.rate,
                           m.max_bitpool, m.channel_mode);
        break;
    }
    case A2DP_SUBEVENT_SIGNALING_DELAY_REPORT:
        printf("DELAY ms=%d\n", avdtp_subevent_signaling_delay_report_get_delay_100us(packet) / 10);
        break;
    case A2DP_SUBEVENT_STREAM_ESTABLISHED:
        status = a2dp_subevent_stream_established_get_status(packet);
        if (status != ERROR_CODE_SUCCESS) {
            printf("ERROR stream status=0x%02x\n", status);
            if (mode != MODE_PLAY) shutdown_with(3);
            break;
        }
        a2dp_source_start_stream(m.a2dp_cid, m.local_seid);
        break;
    case A2DP_SUBEVENT_STREAM_STARTED:
        btstack_run_loop_remove_timer(&deadline);
        printf("STREAMING rate=%d\n", m.rate);
        fflush(stdout);
        start_audio();
        if (avrcp_cid) {
            avrcp_target_set_playback_status(avrcp_cid, AVRCP_PLAYBACK_STATUS_PLAYING);
        } else if (mode == MODE_PLAY) {
            uint16_t cid;
            avrcp_connect(remote, &cid); // as a phone does; the speaker may have opened it already
        }
        break;
    case A2DP_SUBEVENT_STREAMING_CAN_SEND_MEDIA_PACKET_NOW:
        if (m.streaming) send_media();
        break;
    case A2DP_SUBEVENT_STREAM_SUSPENDED:
        printf("SUSPENDED\n");
        stop_audio();
        break;
    case A2DP_SUBEVENT_STREAM_RELEASED:
        stop_audio();
        break;
    case A2DP_SUBEVENT_SIGNALING_CONNECTION_RELEASED:
        stop_audio();
        m.a2dp_cid = 0;
        printf("DISCONNECTED underruns=%u\n", underruns);
        if (mode == MODE_PLAY && !shutting_down) {
            connect_later(RECONNECT_MS);
            break;
        }
        if (!shutting_down) {
            shutting_down = true;
            exit_code = 4;
        }
        hci_power_control(HCI_POWER_OFF);
        break;
    default:
        break;
    }
    fflush(stdout);
}

static void hci_handler(uint8_t packet_type, uint16_t channel, uint8_t *packet, uint16_t size) {
    (void)channel;
    (void)size;
    if (packet_type != HCI_EVENT_PACKET) return;
    bd_addr_t addr;
    switch (hci_event_packet_get_type(packet)) {
    case BTSTACK_EVENT_STATE:
        switch (btstack_event_state_get_state(packet)) {
        case HCI_STATE_WORKING: {
            gap_local_bd_addr(addr);
            mkdir(key_dir, 0700);
            snprintf(tlv_path, sizeof(tlv_path), "%s/keys.tlv", key_dir);
            const btstack_tlv_t *tlv = btstack_tlv_posix_init_instance(&tlv_context, tlv_path);
            btstack_tlv_set_instance(tlv, &tlv_context);
            hci_set_link_key_db(btstack_link_key_db_tlv_get_instance(tlv, &tlv_context));
            printf("READY %s\n", bd_addr_to_str(addr));
            if (mode == MODE_SCAN) {
                gap_inquiry_start(INQUIRY_1280MS);
                arm_deadline(INQUIRY_1280MS * 1280 + 2000);
            } else if (mode == MODE_TONE) {
                arm_deadline(CONNECT_TIMEOUT_MS);
                connect_out();
                if (!m.a2dp_cid) shutdown_with(3);
            } else {
                connect_out();
            }
            break;
        }
        case HCI_STATE_OFF:
            btstack_tlv_posix_deinit(&tlv_context);
            fflush(stdout);
            exit(exit_code);
            break;
        default:
            break;
        }
        break;
    case HCI_EVENT_DISCONNECTION_COMPLETE:
        printf("LINK down reason=0x%02x\n", hci_event_disconnection_complete_get_reason(packet));
        break;
    case HCI_EVENT_CONNECTION_REQUEST:
        hci_event_connection_request_get_bd_addr(packet, addr);
        printf("LINK request from %s\n", bd_addr_to_str(addr));
        break;
    case HCI_EVENT_AUTHENTICATION_COMPLETE:
        printf("LINK authentication status=0x%02x\n", hci_event_authentication_complete_get_status(packet));
        break;
    case HCI_EVENT_PIN_CODE_REQUEST:
        hci_event_pin_code_request_get_bd_addr(packet, addr);
        gap_pin_code_response(addr, "0000");
        break;
    case GAP_EVENT_INQUIRY_RESULT: {
        gap_event_inquiry_result_get_bd_addr(packet, addr);
        printf("DEVICE %s cod=%06" PRIx32, bd_addr_to_str(addr), gap_event_inquiry_result_get_class_of_device(packet));
        if (gap_event_inquiry_result_get_rssi_available(packet))
            printf(" rssi=%d", (int8_t)gap_event_inquiry_result_get_rssi(packet));
        if (gap_event_inquiry_result_get_name_available(packet)) {
            char name[240];
            int len = gap_event_inquiry_result_get_name_len(packet);
            memcpy(name, gap_event_inquiry_result_get_name(packet), len);
            name[len] = 0;
            printf(" name=%s", name);
        }
        printf("\n");
        break;
    }
    case GAP_EVENT_INQUIRY_COMPLETE:
        if (mode == MODE_SCAN) shutdown_with(0);
        break;
    case HCI_EVENT_COMMAND_COMPLETE:
        if (hci_event_command_complete_get_command_opcode(packet) == 0xfc1a)
            printf("ADDRESS set status=0x%02x\n", hci_event_command_complete_get_return_parameters(packet)[0]);
        break;
    default:
        break;
    }
    fflush(stdout);
}

// own_address: the WiFi MAC + 1, so every Dot has its own stable address without a file, but
// with 02 for Amazon's OUI: an Alexa Echo took the Dot under Amazon's OUI as a source, then
// dropped it after 18 s (reason 0x13) and refused it after (0x0f), 20.10.2026.
static bool own_address(bd_addr_t addr) {
    FILE *f = fopen("/sys/class/net/wlan0/address", "r");
    if (!f) return false;
    char line[32] = {0};
    bool ok = fgets(line, sizeof(line), f) && sscanf_bd_addr(line, addr);
    fclose(f);
    if (ok) {
        addr[0] = 0x02;
        addr[5]++;
    }
    return ok;
}

static void on_signal(void) {
    shutdown_with(0);
}

static void usage(void) {
    fprintf(stderr, "usage: btspeaker [-v] [-d keydir] [-b addr] [-n name] scan | tone <addr> [seconds] | play <addr>\n");
    exit(1);
}

int main(int argc, const char *argv[]) {
    int i = 1;
    for (; i + 1 < argc && argv[i][0] == '-'; i += 2) {
        if (strcmp(argv[i], "-v") == 0) {
            absolute_volume = true;
            i--; // no value
            continue;
        }
        if (strcmp(argv[i], "-d") == 0) key_dir = argv[i + 1];
        else if (strcmp(argv[i], "-b") == 0 && sscanf_bd_addr(argv[i + 1], own_addr)) own_addr_set = true;
        else if (strcmp(argv[i], "-n") == 0) local_name = argv[i + 1];
        else usage();
    }
    if (i >= argc) usage();
    if (strcmp(argv[i], "scan") == 0) {
        mode = MODE_SCAN;
    } else if ((strcmp(argv[i], "tone") == 0 || strcmp(argv[i], "play") == 0) && i + 1 < argc) {
        mode = argv[i][0] == 't' ? MODE_TONE : MODE_PLAY;
        if (!sscanf_bd_addr(argv[i + 1], remote)) usage();
        if (mode == MODE_TONE && i + 2 < argc) tone_seconds = atoi(argv[i + 2]);
    } else {
        usage();
    }
    if (!own_addr_set) own_addr_set = own_address(own_addr);

    btstack_memory_init();
    btstack_run_loop_init(btstack_run_loop_posix_get_instance());
    char trace_flag[200];
    snprintf(trace_flag, sizeof(trace_flag), "%s/trace", key_dir);
    if (getenv("BTSPEAKER_DUMP")) {
        hci_dump_init(hci_dump_posix_stdout_get_instance());
    } else if (access(trace_flag, F_OK) == 0 && (trace = fopen("/tmp/btspeaker.trace", "a"))) {
        hci_dump_init(&trace_dump);
        printf("TRACE /tmp/btspeaker.trace\n");
    }

    hci_init(hci_transport_stpbt_instance(), NULL);
    hci_set_chipset(btstack_chipset_stpbt_instance());
    if (own_addr_set) hci_set_bd_addr(own_addr);

    hci_registration.callback = &hci_handler;
    hci_add_event_handler(&hci_registration);

    l2cap_init();
    a2dp_source_init();
    a2dp_source_register_packet_handler(&a2dp_handler);
    avdtp_stream_endpoint_t *ep = a2dp_source_create_stream_endpoint(AVDTP_AUDIO, AVDTP_CODEC_SBC, sbc_capabilities,
                                                                     sizeof(sbc_capabilities), sbc_configuration,
                                                                     sizeof(sbc_configuration));
    if (!ep) {
        printf("ERROR endpoint\n");
        return 1;
    }
    avdtp_set_preferred_sampling_frequency(ep, PREFERRED_RATE);
    m.local_seid = avdtp_local_seid(ep);
    avdtp_source_register_delay_reporting_category(m.local_seid);

    sdp_init();
    a2dp_source_create_sdp_record(sdp_a2dp_source_record, sdp_create_service_record_handle(),
                                  AVDTP_SOURCE_FEATURE_MASK_PLAYER, NULL, NULL);
    sdp_register_service(sdp_a2dp_source_record);

    avrcp_init();
    avrcp_register_packet_handler(&avrcp_handler);
    avrcp_target_init();
    avrcp_target_register_packet_handler(&avrcp_target_handler);
    avrcp_controller_init();
    avrcp_controller_register_packet_handler(&avrcp_controller_handler);
    avrcp_target_create_sdp_record(sdp_avrcp_target_record, sdp_create_service_record_handle(),
                                   AVRCP_FEATURE_MASK_CATEGORY_PLAYER_OR_RECORDER, NULL, NULL);
    sdp_register_service(sdp_avrcp_target_record);
    avrcp_controller_create_sdp_record(sdp_avrcp_controller_record, sdp_create_service_record_handle(),
                                       AVRCP_FEATURE_MASK_CATEGORY_MONITOR_OR_AMPLIFIER, NULL, NULL);
    sdp_register_service(sdp_avrcp_controller_record);
    device_id_create_sdp_record(sdp_device_id_record, sdp_create_service_record_handle(),
                                DEVICE_ID_VENDOR_ID_SOURCE_BLUETOOTH, BLUETOOTH_COMPANY_ID_BLUEKITCHEN_GMBH, 1, 1);
    sdp_register_service(sdp_device_id_record);

    gap_set_local_name(local_name);
    gap_set_class_of_device(0x5a020c); // a phone: sinks take sources of that kind
    gap_discoverable_control(0);
    gap_connectable_control(mode == MODE_PLAY);
    gap_ssp_set_io_capability(SSP_IO_CAPABILITY_NO_INPUT_NO_OUTPUT);
    gap_ssp_set_auto_accept(1);
    gap_set_bondable_mode(1);
    hci_set_inquiry_mode(INQUIRY_MODE_RSSI_AND_EIR);
    hci_set_master_slave_policy(0);

    if (mode == MODE_PLAY && fcntl(3, F_GETFD) != -1) {
        fcntl(3, F_SETFL, fcntl(3, F_GETFL) | O_NONBLOCK);
        btstack_run_loop_set_data_source_fd(&control_source, 3);
        btstack_run_loop_set_data_source_handler(&control_source, &control_ready);
        btstack_run_loop_enable_data_source_callbacks(&control_source, DATA_SOURCE_CALLBACK_READ);
        btstack_run_loop_add_data_source(&control_source);
    }
    if (mode == MODE_PLAY) {
        fcntl(0, F_SETFL, fcntl(0, F_GETFL) | O_NONBLOCK);
        btstack_run_loop_set_data_source_fd(&stdin_source, 0);
        btstack_run_loop_set_data_source_handler(&stdin_source, &stdin_ready);
        btstack_run_loop_enable_data_source_callbacks(&stdin_source, DATA_SOURCE_CALLBACK_READ);
        btstack_run_loop_add_data_source(&stdin_source);
    }

    btstack_signal_register_callback(SIGINT, on_signal);
    btstack_signal_register_callback(SIGTERM, on_signal);
    signal(SIGPIPE, SIG_IGN);
    setvbuf(stdout, NULL, _IOLBF, 0);
    hci_power_control(HCI_POWER_ON);
    btstack_run_loop_execute();
    return exit_code;
}
