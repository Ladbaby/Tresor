package jsplugins

// fault records native contract violations; keep enforcement independent of logging.
func (i *Instance) fault(code string) {
	if i.hostFault == "" {
		i.hostFault = code
	}
	panic(i.vm.NewTypeError("Host contract violation"))
}
