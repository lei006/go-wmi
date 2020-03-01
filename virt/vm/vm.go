package vm

import (
	"fmt"
	"go-wmi/wmi"
	"runtime"
	"strings"

	"go-wmi/utils"

	"github.com/go-ole/go-ole"
	"github.com/pkg/errors"
)

// NewVMManager returns a new Manager type
func NewVMManager() (*Manager, error) {
	w, err := wmi.NewConnection(".", `root\virtualization\v2`)
	if err != nil {
		return nil, err
	}

	// Get virtual machine management service
	svc, err := w.GetOne(VMManagementService, []string{}, []wmi.Query{})
	if err != nil {
		return nil, err
	}

	sw := &Manager{
		con: w,
		svc: svc,
	}
	return sw, nil
}

// Manager manages a VM switch
type Manager struct {
	con *wmi.WMI
	svc *wmi.Result
}

// GetVM returns the virtual machine identified by instanceID
func (m *Manager) GetVM(instanceID string) (*VirtualMachine, error) {
	fields := []string{}
	qParams := []wmi.Query{
		&wmi.AndQuery{
			wmi.QueryFields{
				Key:   "VirtualSystemType",
				Value: VirtualSystemTypeRealized,
				Type:  wmi.Equals},
		},
		&wmi.AndQuery{
			wmi.QueryFields{
				Key:   "VirtualSystemIdentifier",
				Value: instanceID,
				Type:  wmi.Equals},
		},
	}

	result, err := m.con.Gwmi(VirtualSystemSettingDataClass, fields, qParams)
	if err != nil {
		return nil, errors.Wrap(err, "VirtualSystemSettingDataClass")
	}

	vssd, err := result.ItemAtIndex(0)
	if err != nil {
		return nil, errors.Wrap(err, "fetching element")
	}
	cs, err := vssd.Get("associators_", nil, ComputerSystemClass)
	if err != nil {
		return nil, errors.Wrap(err, "getting ComputerSystemClass")
	}
	elem, err := cs.Elements()
	if err != nil || len(elem) == 0 {
		return nil, errors.Wrap(err, "getting elements")
	}
	return &VirtualMachine{
		mgr:                m,
		activeSettingsData: vssd,
		computerSystem:     elem[0],
	}, nil
}

// ListVM returns a list of virtual machines
func (m *Manager) ListVM() ([]*VirtualMachine, error) {
	fields := []string{}
	qParams := []wmi.Query{
		&wmi.AndQuery{
			wmi.QueryFields{
				Key:   "VirtualSystemType",
				Value: VirtualSystemTypeRealized,
				Type:  wmi.Equals},
		},
	}

	result, err := m.con.Gwmi(VirtualSystemSettingDataClass, fields, qParams)
	if err != nil {
		return nil, errors.Wrap(err, "VirtualSystemSettingDataClass")
	}

	elements, err := result.Elements()
	if err != nil {
		return nil, errors.Wrap(err, "Elements")
	}
	vms := make([]*VirtualMachine, len(elements))
	for idx, val := range elements {
		cs, err := val.Get("associators_", nil, ComputerSystemClass)
		if err != nil {
			return nil, errors.Wrap(err, "getting ComputerSystemClass")
		}
		elem, err := cs.Elements()
		if err != nil || len(elem) == 0 {
			return nil, errors.Wrap(err, "getting elements")
		}
		vms[idx] = &VirtualMachine{
			mgr:                m,
			activeSettingsData: val,
			computerSystem:     elem[0],
		}
	}
	return vms, nil
}

// CreateVM creates a new virtual machine
func (m *Manager) CreateVM(name string, memoryMB int64, cpus int, limitCPUFeatures bool, notes []string, generation GenerationType) (*VirtualMachine, error) {
	vmSettingsDataInstance, err := m.con.Get(VirtualSystemSettingDataClass)
	if err != nil {
		return nil, err
	}

	newVMInstance, err := vmSettingsDataInstance.Get("SpawnInstance_")
	if err != nil {
		return nil, errors.Wrap(err, "calling SpawnInstance_")
	}

	if err := newVMInstance.Set("ElementName", name); err != nil {
		return nil, errors.Wrap(err, "Set ElementName")
	}
	if err := newVMInstance.Set("VirtualSystemSubType", string(generation)); err != nil {
		return nil, errors.Wrap(err, "Set VirtualSystemSubType")
	}
	if notes != nil && len(notes) > 0 {
		// Don't ask...
		// Well, ok...if you must. The Msvm_VirtualSystemSettingData has a Notes
		// property of type []string. But in reality, it only cares about the first
		// element of that array. So we join the notes into one newline delimited
		// string, and set that as the first and only element in a new []string{}
		vmNotes := []string{strings.Join(notes, "\n")}
		if err := newVMInstance.Set("Notes", vmNotes); err != nil {
			return nil, errors.Wrap(err, "Set Notes")
		}
	}

	vmText, err := newVMInstance.GetText(1)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get VM instance XML")
	}

	jobPath := ole.VARIANT{}
	resultingSystem := ole.VARIANT{}
	jobState, err := m.svc.Get("DefineSystem", vmText, nil, nil, &resultingSystem, &jobPath)
	if err != nil {
		return nil, errors.Wrap(err, "calling DefineSystem")
	}
	if jobState.Value().(int32) == wmi.JobStatusStarted {
		err := wmi.WaitForJob(jobPath.Value().(string))
		if err != nil {
			return nil, errors.Wrap(err, "waiting for job")
		}
	}

	// The resultingSystem value for DefineSystem is always a string containing the
	// location of the newly created resource
	locationURI := resultingSystem.Value().(string)
	loc, err := wmi.NewLocation(locationURI)
	if err != nil {
		return nil, errors.Wrap(err, "getting location")
	}

	result, err := loc.GetResult()
	if err != nil {
		return nil, errors.Wrap(err, "getting result")
	}

	// The name field of the returning class is actually the InstanceID...
	id, err := result.GetProperty("Name")
	if err != nil {
		return nil, errors.Wrap(err, "fetching VM ID")
	}

	vm, err := m.GetVM(id.Value().(string))
	if err != nil {
		return nil, errors.Wrap(err, "fetching VM")
	}

	if err := vm.SetMemory(memoryMB); err != nil {
		return nil, errors.Wrap(err, "setting memory limit")
	}

	if err := vm.SetNumCPUs(cpus); err != nil {
		return nil, errors.Wrap(err, "setting CPU limit")
	}

	bootOrder := []int32{
		int32(BootHDD),
		int32(BootPXE),
		int32(BootCDROM),
		int32(BootFloppy),
	}

	if err := vm.SetBootOrder(bootOrder); err != nil {
		return nil, errors.Wrap(err, "setting boot order")
	}

	return vm, nil
}

// Release closes the WMI connection associated with this
// Manager
func (m *Manager) Release() {
	m.con.Close()
}

// VirtualMachine represents a single virtual machine
type VirtualMachine struct {
	mgr *Manager

	activeSettingsData *wmi.Result
	computerSystem     *wmi.Result
}

// Name returns the current name of this virtual machine
func (v *VirtualMachine) Name() (string, error) {
	name, err := v.computerSystem.GetProperty("ElementName")
	if err != nil {
		return "", errors.Wrap(err, "getting ElementName")
	}
	return name.Value().(string), nil
}

// ID returns the instance ID of this Virtual machine
func (v *VirtualMachine) ID() (string, error) {
	id, err := v.activeSettingsData.GetProperty("VirtualSystemIdentifier")
	if err != nil {
		return "", errors.Wrap(err, "fetching VM ID")
	}
	return id.Value().(string), nil
}

// AttachDisks attaches the supplied disks, to this virtual machine
func (v *VirtualMachine) AttachDisks(disks []string) error {
	return nil
}

// SetBootOrder sets the VM boot order
func (v *VirtualMachine) SetBootOrder(bootOrder []int32) error {
	if err := v.activeSettingsData.Set("BootOrder", bootOrder); err != nil {
		return errors.Wrap(err, "Set BootOrder")
	}

	vmText, err := v.activeSettingsData.GetText(1)

	jobPath := ole.VARIANT{}
	jobState, err := v.mgr.svc.Get("ModifySystemSettings", vmText, &jobPath)
	if err != nil {
		return errors.Wrap(err, "calling ModifySystemSettings")
	}
	if jobState.Value().(int32) == wmi.JobStatusStarted {
		err := wmi.WaitForJob(jobPath.Value().(string))
		if err != nil {
			return errors.Wrap(err, "waiting for job")
		}
	}
	return nil
}

func (v *VirtualMachine) modifyResourceSettings(settings []string) error {
	jobPath := ole.VARIANT{}
	resultingSystem := ole.VARIANT{}
	jobState, err := v.mgr.svc.Get("ModifyResourceSettings", settings, &resultingSystem, &jobPath)
	if err != nil {
		return errors.Wrap(err, "calling ModifyResourceSettings")
	}
	if jobState.Value().(int32) == wmi.JobStatusStarted {
		err := wmi.WaitForJob(jobPath.Value().(string))
		if err != nil {
			return errors.Wrap(err, "waiting for job")
		}
	}
	return nil
}

// SetMemory sets the virtual machine memory allocation
func (v *VirtualMachine) SetMemory(memoryMB int64) error {
	memorySettingsResults, err := v.activeSettingsData.Get("associators_", nil, MemorySettingDataClass)
	if err != nil {
		return errors.Wrap(err, "getting MemorySettingDataClass")
	}

	memorySettings, err := memorySettingsResults.ItemAtIndex(0)
	if err != nil {
		return errors.Wrap(err, "ItemAtIndex")
	}

	if err := memorySettings.Set("Limit", memoryMB); err != nil {
		return errors.Wrap(err, "Limit")
	}

	if err := memorySettings.Set("Reservation", memoryMB); err != nil {
		return errors.Wrap(err, "Reservation")
	}

	if err := memorySettings.Set("VirtualQuantity", memoryMB); err != nil {
		return errors.Wrap(err, "VirtualQuantity")
	}

	memText, err := memorySettings.GetText(1)
	if err != nil {
		return errors.Wrap(err, "Failed to get VM instance XML")
	}

	return v.modifyResourceSettings([]string{memText})
}

// SetNumCPUs sets the number of CPU cores on the VM
func (v *VirtualMachine) SetNumCPUs(cpus int) error {
	hostCpus := runtime.NumCPU()
	if hostCpus < cpus {
		return fmt.Errorf("Number of cpus exceeded available host resources")
	}

	procSettingsResults, err := v.activeSettingsData.Get("associators_", nil, ProcessorSettingDataClass)
	if err != nil {
		return errors.Wrap(err, "getting ProcessorSettingDataClass")
	}

	procSettings, err := procSettingsResults.ItemAtIndex(0)
	if err != nil {
		return errors.Wrap(err, "ItemAtIndex")
	}

	if err := procSettings.Set("VirtualQuantity", uint64(cpus)); err != nil {
		return errors.Wrap(err, "VirtualQuantity")
	}

	if err := procSettings.Set("Reservation", cpus); err != nil {
		return errors.Wrap(err, "Reservation")
	}

	if err := procSettings.Set("Limit", 100000); err != nil {
		return errors.Wrap(err, "Limit")
	}

	procText, err := procSettings.GetText(1)
	if err != nil {
		return errors.Wrap(err, "Failed to get VM instance XML")
	}
	return v.modifyResourceSettings([]string{procText})
}

// SetPowerState sets the desired power state on a virtual machine.
func (v *VirtualMachine) SetPowerState(state PowerState) error {
	jobPath := ole.VARIANT{}
	jobState, err := v.computerSystem.Get("RequestStateChange", uint16(state), &jobPath)
	if err != nil {
		return errors.Wrap(err, "calling RequestStateChange")
	}
	if jobState.Value().(int32) == wmi.JobStatusStarted {
		err = wmi.WaitForJob(jobPath.Value().(string))
		if err != nil {
			return errors.Wrap(err, "waiting for job")
		}
	}
	return nil
}

func (v *VirtualMachine) getSCSIResourceAllocSettings() (*wmi.Result, error) {
	// ResourceAllocSettingDataClass

	qParams := []wmi.Query{
		&wmi.AndQuery{
			wmi.QueryFields{
				Key:   "ResourceSubType",
				Value: SCSIControllerResSubType,
				Type:  wmi.Equals,
			},
		},
		&wmi.AndQuery{
			wmi.QueryFields{
				Key:   "InstanceID",
				Value: "%\\\\Default",
				Type:  wmi.Like,
			},
		},
	}
	settingsDataResults, err := v.mgr.con.Gwmi(ResourceAllocSettingDataClass, []string{}, qParams)
	if err != nil {
		return nil, errors.Wrap(err, "getting ResourceAllocSettingDataClass")
	}
	settingsData, err := settingsDataResults.ItemAtIndex(0)
	if err != nil {
		return nil, errors.Wrap(err, "getting result")
	}
	return settingsData, nil
}

func (v *VirtualMachine) addResourceSetting(settingsData []string) ([]string, error) {
	vmPath, err := v.computerSystem.Path()
	if err != nil {
		return nil, errors.Wrap(err, "vm Path()")
	}
	jobPath := ole.VARIANT{}
	resultingSystem := ole.VARIANT{}
	jobState, err := v.mgr.svc.Get("AddResourceSettings", vmPath, settingsData, &resultingSystem, &jobPath)
	if err != nil {
		return nil, errors.Wrap(err, "calling ModifyResourceSettings")
	}

	if jobState.Value().(int32) == wmi.JobStatusStarted {
		err := wmi.WaitForJob(jobPath.Value().(string))
		if err != nil {
			return nil, errors.Wrap(err, "waiting for job")
		}
	}
	safeArrayConversion := resultingSystem.ToArray()
	valArray := safeArrayConversion.ToValueArray()
	if len(valArray) == 0 {
		return nil, fmt.Errorf("no resource in resultingSystem value")
	}
	resultingSystems := make([]string, len(valArray))
	for idx, val := range valArray {
		resultingSystems[idx] = val.(string)
	}
	return resultingSystems, nil
}

// CreateNewSCSIController will create a new ISCSI controller on this VM
func (v *VirtualMachine) CreateNewSCSIController() (string, error) {
	resData, err := v.getSCSIResourceAllocSettings()
	if err != nil {
		return "", errors.Wrap(err, "getSCSIResourceAllocSettings")
	}
	newID, err := utils.UUID4()
	if err != nil {
		return "", errors.Wrap(err, "UUID4")
	}
	if err := resData.Set("VirtualSystemIdentifiers", []string{fmt.Sprintf("{%s}", newID)}); err != nil {
		return "", errors.Wrap(err, "VirtualSystemIdentifiers")
	}

	dataText, err := resData.GetText(1)
	if err != nil {
		return "", errors.Wrap(err, "GetText")
	}

	resCtrl, err := v.addResourceSetting([]string{dataText})
	if err != nil {
		return "", errors.Wrap(err, "addResourceSetting")
	}
	return resCtrl[0], nil
}

func (v *VirtualMachine) getResourceOfType(subType string) (string, error) {
	settingClasses, err := v.activeSettingsData.Get("associators_", nil, ResourceAllocSettingDataClass)
	if err != nil {
		return "", errors.Wrap(err, "getting ResourceAllocSettingDataClass")
	}
	settingElements, err := settingClasses.Elements()
	if err != nil {
		return "", errors.Wrap(err, "fetching elements")
	}
	for _, val := range settingElements {
		resSubtype, err := val.GetProperty("ResourceSubType")
		if err != nil {
			continue
		}
		if resSubtype.Value().(string) == subType {
			pth, err := val.Path()
			if err != nil {
				return "", errors.Wrap(err, "SCSIControllerResSubType path_")
			}
			return pth, nil
		}
	}
	return "", wmi.ErrNotFound
}

// GetOrCreateSCSIController will look for an ISCSI controller on the VM. If one is
// present, it will be returned. If not, one will be created then returned.
func (v *VirtualMachine) GetOrCreateSCSIController() (string, error) {
	res, err := v.getResourceOfType(SCSIControllerResSubType)
	if err != nil {
		// If we get any other error than not found, we return
		if err != wmi.ErrNotFound {
			return "", errors.Wrap(err, "getResourceOfType SCSIControllerResSubType")
		}
	} else {
		return res, nil
	}

	// Need to create one
	ctrl, err := v.CreateNewSCSIController()
	if err != nil {
		return "", errors.Wrap(err, "CreateNewSCSIController")
	}
	return ctrl, nil
}
