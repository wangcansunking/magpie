package wan

// RelayRecoveryTimeoutMessage is shared with the desktop's one-shot failure
// notification. The bundled relay helper owns the fixed five-minute recovery deadline.
const RelayRecoveryTimeoutMessage = "WAN could not reconnect for 5 minutes; check the network and start WAN again"
