#pragma once

#include "btstack_chipset.h"
#include "hci_transport.h"

const hci_transport_t *hci_transport_stpbt_instance(void);
const btstack_chipset_t *btstack_chipset_stpbt_instance(void);
