# btspeaker for the Echo Dot 2 (Alpine armv7, static). Run through build.sh, which fetches BTstack.
BTSTACK ?= btstack

SRC = main.c hci_transport_stpbt.c \
	$(addprefix $(BTSTACK)/src/, ad_parser.c btstack_crypto.c btstack_linked_list.c btstack_memory.c \
		btstack_memory_pool.c btstack_ring_buffer.c btstack_run_loop.c btstack_tlv.c btstack_util.c \
		hci.c hci_cmd.c hci_dump.c hci_event.c hci_event_builder.c l2cap.c l2cap_signaling.c) \
	$(addprefix $(BTSTACK)/src/classic/, a2dp.c a2dp_source.c avdtp.c avdtp_acceptor.c avdtp_initiator.c \
		avdtp_sink.c avdtp_source.c avdtp_util.c avrcp.c avrcp_controller.c avrcp_target.c \
		btstack_link_key_db_tlv.c btstack_sbc_bluedroid.c device_id_server.c \
		btstack_sbc_encoder_bluedroid.c sdp_client.c sdp_server.c sdp_util.c) \
	$(addprefix $(BTSTACK)/3rd-party/bluedroid/encoder/srce/, sbc_analysis.c sbc_dct.c sbc_dct_coeffs.c \
		sbc_enc_bit_alloc_mono.c sbc_enc_bit_alloc_ste.c sbc_enc_coeffs.c sbc_encoder.c sbc_packing.c) \
	$(addprefix $(BTSTACK)/platform/posix/, btstack_run_loop_posix.c btstack_signal.c btstack_tlv_posix.c \
		hci_dump_posix_stdout.c)

CFLAGS = -O2 -Wall -I. -I$(BTSTACK)/src -I$(BTSTACK)/src/classic -I$(BTSTACK)/platform/posix \
	-I$(BTSTACK)/platform/embedded -I$(BTSTACK)/3rd-party/bluedroid/encoder/include \
	-I$(BTSTACK)/3rd-party/bluedroid/decoder/include -I$(BTSTACK)/3rd-party/yxml \
	-ffunction-sections -fdata-sections
LDFLAGS = -static -Wl,--gc-sections -s -lm

btspeaker: $(SRC) btstack_config.h hci_transport_stpbt.h
	$(CC) $(CFLAGS) -o $@ $(SRC) $(LDFLAGS)

clean:
	rm -f btspeaker
