// BTstack configuration for btspeaker on the Echo Dot 2: Classic only, A2DP source.

#ifndef BTSTACK_CONFIG_H
#define BTSTACK_CONFIG_H

#define HAVE_ASSERT
#define HAVE_MALLOC
#define HAVE_POSIX_FILE_IO
#define HAVE_POSIX_TIME

#define ENABLE_CLASSIC
#define ENABLE_LOG_ERROR // on stdout, into Jarvis's log
#define ENABLE_PRINTF_HEXDUMP // BTSPEAKER_DUMP=1: the HCI trace on stdout

#define HCI_ACL_PAYLOAD_SIZE (1691 + 4)
#define HCI_INCOMING_PRE_BUFFER_SIZE 14

#define NVM_NUM_LINK_KEYS 8

#endif
