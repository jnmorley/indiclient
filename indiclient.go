// Package indiclient is a pure Go implementation of an indi client. It supports indiserver version 1.7.
//
// See http://indilib.org/develop/developer-manual/106-client-development.html
//
// See http://www.clearskyinstitute.com/INDI/INDI.pdf
//
// One of the awesome, but sometimes infuriating features of the INDI protocol is that if a device receives
// a command it doesn't understand, it is under no obligation to respond, and usually won't. This can make
// debugging difficult, because you aren't always sure if you are just sending the command incorrectly or
// if there is something else wrong. This library tries to alleviate that by checking parameters to all
// calls and will return an error if something doesn't look right.
package indiclient

// TODO: Handle device timeouts

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rickbassham/logging"
	"github.com/spf13/afero"
)

var (
	// ErrDeviceNotFound is returned when a call cannot find a device.
	ErrDeviceNotFound = errors.New("device not found")

	// ErrPropertyBusy is returned when the client tries to change a busy property.
	ErrPropertyStateBusy = errors.New("Tried to set a busy property")

	// ErrPropertyNotFound is returned when a call cannot find a property.
	ErrPropertyNotFound = errors.New("property not found")

	// ErrPropertyValueNotFound is returned when a call cannot find a property value.
	ErrPropertyValueNotFound = errors.New("property value not found")

	// ErrPropertyReadOnly is returned when an attempt to change a read-only property was made.
	ErrPropertyReadOnly = errors.New("property read only")

	// ErrPropertyWithoutDevice is returned when an attempt to GetProperties specifies a property but no device.
	ErrPropertyWithoutDevice = errors.New("property specified without device")

	// ErrInvalidBlobEnable is returned when a value other than Only, Also, Never is specified for BlobEnable.
	ErrInvalidBlobEnable = errors.New("invalid BlobEnable value")

	// ErrBlobNotFound is returned when an attempt to read a blob value is made but none are found
	ErrBlobNotFound = errors.New("blob not found")
)

// PropertyState represents the current state of a property. "Idle", "Ok", "Busy", or "Alert".
type PropertyState string

const (
	// PropertyStateIdle represents a property that is "Idle". This is recommended to be displayed as Gray.
	PropertyStateIdle = PropertyState("Idle")
	// PropertyStateOk represents a property that is "Ok". This is recommended to be displayed as Green.
	PropertyStateOk = PropertyState("Ok")
	// PropertyStateBusy represents a property that is "Busy". This is recommended to be displayed as Yellow.
	PropertyStateBusy = PropertyState("Busy")
	// PropertyStateAlert represents a property that is "Alert". This is recommended to be displayed as Red.
	PropertyStateAlert = PropertyState("Alert")
)

// SwitchState reprensents the current state of a switch value. "On" or "Off".
type SwitchState string

const (
	// SwitchStateOff represents a switch that is "Off".
	SwitchStateOff = SwitchState("Off")
	// SwitchStateOn represents a switch that is "On".
	SwitchStateOn = SwitchState("On")
)

// SwitchRule represents how a switch state can exist relative to the other switches in the vector. "OneOfMany", "AtMostOne", or "AnyOfMany".
type SwitchRule string

const (
	// SwitchRuleOneOfMany represents a switch that must have one switch in a vector active at a time.
	SwitchRuleOneOfMany = SwitchRule("OneOfMany")
	// SwitchRuleAtMostOne represents a switch that must have no more than one switch in a vector active at a time.
	SwitchRuleAtMostOne = SwitchRule("AtMostOne")
	// SwitchRuleAnyOfMany represents a switch that may have any number of switches in a vector active at a time.
	SwitchRuleAnyOfMany = SwitchRule("AnyOfMany")
)

// PropertyPermission represents a permission hint for the client. "ro", "wo", or "rw".
type PropertyPermission string

const (
	// PropertyPermissionReadOnly represents a property that is Read-Only.
	PropertyPermissionReadOnly = PropertyPermission("ro")
	// PropertyPermissionWriteOnly represents a property that is Write-Only.
	PropertyPermissionWriteOnly = PropertyPermission("wo")
	// PropertyPermissionReadWrite represents a property that is Read-Write.
	PropertyPermissionReadWrite = PropertyPermission("rw")
)

// BlobEnable represents whether BLOB's should be sent to this client.
type BlobEnable string

const (
	// BlobEnableNever (default) represents that the current client should not be sent any BLOB's for a device.
	BlobEnableNever = BlobEnable("Never")
	// BlobEnableAlso represents that the current client should be sent any BLOB's for a device in addition to the normal INDI commands.
	BlobEnableAlso = BlobEnable("Also")
	// BlobEnableOnly represents that the current client should only be sent any BLOB's for a device.
	BlobEnableOnly = BlobEnable("Only")
)

// Dialer allows the client to connect to an INDI server.
type Dialer interface {
	Dial(network, address string) (io.ReadWriteCloser, error)
}

// NetworkDialer is an implementation of Dialer that uses the built-in net package.
type NetworkDialer struct{}

// Dial connects to the address on the named network.
func (NetworkDialer) Dial(network, address string) (io.ReadWriteCloser, error) {
	return net.Dial(network, address)
}

// INDIClient is the struct used to keep a connection alive to an indiserver.
type INDIClient struct {
	log        logging.Logger
	dialer     Dialer
	fs         afero.Fs
	bufferSize int

	conn io.ReadWriteCloser

	write chan interface{}
	read  chan interface{}
	writeReturn chan error

	rwm         *sync.RWMutex //Protects devices structure
	devices     map[string]Device
	blobStreams sync.Map
}

// NewINDIClient creates a client to connect to an INDI server.
func NewINDIClient(log logging.Logger, dialer Dialer, fs afero.Fs, bufferSize int) *INDIClient {
	return &INDIClient{
		log:         log,
		dialer:      dialer,
		devices:     make(map[string]Device),
		blobStreams: sync.Map{},
		fs:          fs,
		bufferSize:  bufferSize,
		rwm:         &sync.RWMutex{},
	}
}

// Connect dials to create a connection to address. address should be in the format that the provided Dialer expects.
func (c *INDIClient) Connect(network, address string) error {
	conn, err := c.dialer.Dial(network, address)
	if err != nil {
		return err
	}

	// Clear out all devices
	c.rwm.Lock()
	c.delProperty(&DelProperty{})
	c.rwm.Unlock()
	c.conn = conn

	c.read = make(chan interface{}, c.bufferSize)
	c.write = make(chan interface{}, c.bufferSize) 

	c.startRead()
	c.startWrite()

	return nil
}

// Disconnect clears out all devices from memory, closes the connection, and closes the read and write channels.
func (c *INDIClient) Disconnect() error {
	// Clear out all devices
	c.rwm.Lock()
	c.delProperty(&DelProperty{})
	c.rwm.Unlock()

	if c.conn == nil {
		return nil
	}

	err := c.conn.Close()
	c.conn = nil

	if c.read != nil {
		close(c.read)
		c.read = nil
	}

	if c.write != nil {
		close(c.write)
		c.write = nil
	}

	return err
}

// IsConnected returns true if the client is currently connected to an INDI server. Otherwise, returns false.
func (c *INDIClient) IsConnected() bool {
	if c.conn != nil {
		return true
	}

	return false
}

// Devices returns the current list of INDI devices with their current state.
func (c *INDIClient) Devices() []string {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	devices := []string{}

	for key, _ := range c.devices {
		devices = append(devices, key)
	}
	return devices
}

// GetBlob finds a BLOB with the given deviceName, propName, blobName. Be sure to close rdr when you are done with it.
func (c *INDIClient) GetBlob(deviceName, propName, blobName string) (rdr io.ReadCloser, fileName string, length int64, err error) {
	c.rwm.Lock()
	defer c.rwm.Unlock()

	device, err := c.findDevice(deviceName)
	if err != nil {
		return
	}

	prop, ok := device.BlobProperties[propName]
	if !ok {
		err = ErrPropertyNotFound
		return
	}

	val, ok := prop.Values[blobName]
	if !ok {
		err = ErrPropertyValueNotFound
		return
	}

	if val.Size == 0 || val.Name == "" {
		err = ErrBlobNotFound
		return
	}

	rdr, err = c.fs.Open(val.Value)
	if err != nil {
		return
	}

	fileName = filepath.Base(val.Value)

	length = val.Size
	// This method should only work once per blob, so the blob value and size are reset
	val.Value = ""
	val.Size = 0

	device.BlobProperties[propName] = prop
	
	c.devices[deviceName] = device
	return
}

func (c *INDIClient) BlobAvailable(deviceName, propName, blobName string) bool {
	c.rwm.RLock()
	defer c.rwm.RUnlock()

	device, err := c.findDevice(deviceName)
	if err != nil {
		return false
	}

	prop, ok := device.BlobProperties[propName]
	if !ok {
		return false
	}

	val, ok := prop.Values[blobName]
	if !ok {
		return false
	}

	if val.Size == 0 || val.Name == "" {
		return false
	}

	return true
}

// GetBlobStream finds a BLOB with the given deviceName, propName, blobName. This will return an io.Pipe that can stream the BLOBs that are received from the indiserver.
// The client will keep track of all open streams and write to them as blobs are received from indiserver. Remember to call CloseBlobStream when you are done. If you don't,
// all blobs received for that device, property, blob will fail to write once the reader is closed.
func (c *INDIClient) GetBlobStream(deviceName, propName, blobName string) (rdr io.ReadCloser, id string, err error) {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return
	}

	prop, ok := device.BlobProperties[propName]
	if !ok {
		err = ErrPropertyNotFound
		return
	}

	_, ok = prop.Values[blobName]
	if !ok {
		err = ErrPropertyValueNotFound
		return
	}

	guid := uuid.New()
	id = guid.String()

	key := fmt.Sprintf("%s_%s_%s", deviceName, propName, blobName)

	r, w := io.Pipe()

	rdr = r

	writers := map[string]io.Writer{}

	if ws, ok := c.blobStreams.Load(key); ok {
		writers = ws.(map[string]io.Writer)
	}

	writers[id] = w

	c.blobStreams.Store(key, writers)

	return
}

// CloseBlobStream closes the blob stream created by GetBlobStream.
func (c *INDIClient) CloseBlobStream(deviceName, propName, blobName string, id string) (err error) {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return
	}

	prop, ok := device.BlobProperties[propName]
	if !ok {
		err = ErrPropertyNotFound
		return
	}

	_, ok = prop.Values[blobName]
	if !ok {
		err = ErrPropertyValueNotFound
		return
	}
	key := fmt.Sprintf("%s_%s_%s", deviceName, propName, blobName)

	if ws, ok := c.blobStreams.Load(key); ok {
		writers := ws.(map[string]io.Writer)

		if w, ok := writers[id]; ok {
			w.(io.WriteCloser).Close()

			delete(writers, id)

			c.blobStreams.Store(key, writers)
		}
	}

	return
}

// GetProperties sends a command to the INDI server to retreive the property definitions for the given deviceName and propName.
// deviceName and propName are optional.
func (c *INDIClient) GetProperties(deviceName, propName string) error {
	if len(propName) > 0 && len(deviceName) == 0 {
		return ErrPropertyWithoutDevice
	}

	cmd := GetProperties{
		Version: "1.7",
		Device:  deviceName,
		Name:    propName,
	}

	c.write <- cmd

	return nil
}

// Probes the client to check if a text property is set
func (c *INDIClient) TextPropertySet(deviceName, propName string) bool {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return false
	}

	_, ok := device.TextProperties[propName]
	if !ok {
		return false
	}

	return true
	
}

// Probes the client to check if a number property is set
func (c *INDIClient) NumberPropertySet(deviceName, propName string) bool {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return false
	}

	_, ok := device.NumberProperties[propName]
	if !ok {
		return false
	}
	return true
}

// Probes the client to check if a switch property is set
func (c *INDIClient) SwitchPropertySet(deviceName, propName string) bool {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return false
	}

	_, ok := device.SwitchProperties[propName]
	if !ok {
		return false
	}

	return true
}

// Probes the client to check if a blob property is set
func (c *INDIClient) BlobPropertySet(deviceName, propName string) bool {
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return false
	}

	_, ok := device.BlobProperties[propName]
	if !ok {
		return false
	}

	return true
}

// GetText finds a TextValue with the given deviceName, propName, TextName.
func (c *INDIClient) GetText(deviceName, propName, textName string) (TextValue, error){
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return TextValue{}, ErrDeviceNotFound
	}

	prop, ok := device.TextProperties[propName]
	if !ok {
		return TextValue{}, ErrPropertyNotFound
	}

	if val, ok := prop.Values[textName]; ok {
		return val, nil
	}

	return TextValue{}, ErrPropertyValueNotFound
}

// GetNumber finds a NumberValue with the given deviceName, propName, NumberName.
func (c *INDIClient) GetNumber(deviceName, propName, numberName string) (NumberValue, error){
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return NumberValue{}, ErrDeviceNotFound
	}

	prop, ok := device.NumberProperties[propName]
	if !ok {
		return NumberValue{}, ErrPropertyNotFound
	}

	if val, ok := prop.Values[numberName]; ok {
		return val, nil
	}

	return NumberValue{}, ErrPropertyValueNotFound
}

// GetSwitch finds a SwitchValue with the given deviceName, propName, SwitchName.
func (c *INDIClient) GetSwitch(deviceName, propName, switchName string) (SwitchValue, error){
	c.rwm.RLock()
	defer c.rwm.RUnlock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return SwitchValue{}, ErrDeviceNotFound
	}

	prop, ok := device.SwitchProperties[propName]
	if !ok {
		return SwitchValue{}, ErrPropertyNotFound
	}

	if val, ok := prop.Values[switchName]; ok {
		return val, nil
	}

	return SwitchValue{}, ErrPropertyValueNotFound
}

// EnableBlob sends a command to the INDI server to enable/disable BLOBs for the current connection.
// It is recommended to enable blobs on their own client, and keep the main connection clear of large transfers.
// By default, BLOBs are NOT enabled.
func (c *INDIClient) EnableBlob(deviceName, propName string, val BlobEnable) error {
	if val != BlobEnableAlso && val != BlobEnableNever && val != BlobEnableOnly {
		return ErrInvalidBlobEnable
	}

	_, err := c.findDevice(deviceName)
	if err != nil {
		return err
	}

	cmd := EnableBlob{
		Device: deviceName,
		Name:   propName,
		Value:  val,
	}

	c.write <- cmd

	return nil
}

// SetTextValue sends a command to the INDI server to change the value of a textVector.
// Waits to return until the state of the vector is ok.
func (c *INDIClient) SetTextValue(deviceName, propName string, textNames, textValues []string) error {
	if len(textNames) != len(textValues) {
		return errors.New("len(textNames) must be equal to len(textValues)")
	}
	c.rwm.Lock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		c.rwm.Unlock()
		return err
	}

	prop, ok := device.TextProperties[propName]
	if !ok {
		c.rwm.Unlock()
		return ErrPropertyNotFound
	}

	if prop.State == PropertyStateBusy {
		c.rwm.Unlock()
		return ErrPropertyStateBusy
	}

	if prop.Permissions == PropertyPermissionReadOnly {
		c.rwm.Unlock()
		return ErrPropertyReadOnly
	}

	for _, textName := range textNames {
		_, ok = prop.Values[textName]
		if !ok {
			c.rwm.Unlock()
			return ErrPropertyValueNotFound
		}
	}

	prop.State = PropertyStateBusy
	
	device.TextProperties[propName] = prop

	c.devices[deviceName] = device

	texts := []OneText{}
	for index, name := range textNames {
		texts = append(texts, OneText{
			Name: name,
			Value: textValues[index],
		})
	}

	cmd := NewTextVector{
		Device: deviceName,
		Name:   propName,
		Texts: texts,
	}

	c.rwm.Unlock()

	c.write <- cmd
	
	var state PropertyState
	for {
		c.rwm.RLock()
		state = c.devices[deviceName].TextProperties[propName].State
		c.rwm.RUnlock()
		if state == PropertyStateOk {
			break
		}
		if state == PropertyStateAlert {
			return errors.New("Unable to set text property: " + prop.Name)
		}
	}

	return nil
}

// SetNumberValue sends a command to the INDI server to change the value of a numberVector.
func (c *INDIClient) SetNumberValue(deviceName, propName string, numberNames, numberValues []string) error {
	if len(numberNames) != len(numberValues) {
		return errors.New("len(numberNames) must be equal to len(numberValues)")
	}
	c.rwm.Lock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		c.rwm.Unlock()
		return err
	}

	prop, ok := device.NumberProperties[propName]
	if !ok {
		c.rwm.Unlock()
		return ErrPropertyNotFound
	}

	if prop.State == PropertyStateBusy {
		c.rwm.Unlock()
		return ErrPropertyStateBusy
	}

	if prop.Permissions == PropertyPermissionReadOnly {
		c.rwm.Unlock()
		return ErrPropertyReadOnly
	}
	for _, numberName := range numberNames {
		_, ok = prop.Values[numberName]
		if !ok {
			c.rwm.Unlock()
			return ErrPropertyValueNotFound
		}
	}

	prop.State = PropertyStateBusy

	device.NumberProperties[propName] = prop

	c.devices[deviceName] = device

	numbers := []OneNumber{}
	for index, name := range numberNames {
		numbers = append(numbers, OneNumber{
			Name: name,
			Value: numberValues[index],
		})
	}
	
	cmd := NewNumberVector{
		Device: deviceName,
		Name:   propName,
		Numbers: numbers,
	}
	c.rwm.Unlock()
	c.write <- cmd
	var state PropertyState
	for {
		c.rwm.RLock()
		state = c.devices[deviceName].NumberProperties[propName].State
		c.rwm.RUnlock()
		if state == PropertyStateOk {
			break
		}
		if state == PropertyStateAlert {
			return errors.New("Unable to set number property: " + prop.Name)
		}
	}

	return nil
}

// SetSwitchValue sends a command to the INDI server to change the value of a switchVector.
// Note that you will ususally set the desired property on SwitchStateOn, and let the device
// decide how to switch the other values off.
func (c *INDIClient) SetSwitchValue(deviceName, propName string, switchNames []string, switchValues []SwitchState) error {
	if len(switchNames) != len(switchValues) {
		return errors.New("len(switchNames) must be equal to len(switchValues)")
	}
	c.rwm.Lock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		return err
	}

	prop, ok := device.SwitchProperties[propName]
	if !ok {
		c.rwm.Unlock()
		return ErrPropertyNotFound
	}

	if prop.State == PropertyStateBusy {
		c.rwm.Unlock()
		return ErrPropertyStateBusy
	}

	if prop.Permissions == PropertyPermissionReadOnly {
		c.rwm.Unlock()
		return ErrPropertyReadOnly
	}

	for _, switchName := range switchNames {
		_, ok = prop.Values[switchName]
		if !ok {
			c.rwm.Unlock()
			return ErrPropertyValueNotFound
		}
	}

	prop.State = PropertyStateBusy

	device.SwitchProperties[propName] = prop

	c.devices[deviceName] = device

	switches := []OneSwitch{}
	for index, name := range switchNames {
		switches = append(switches, OneSwitch{
			Name: name,
			Value: switchValues[index],
		})
	}
	cmd := NewSwitchVector{
		Device: deviceName,
		Name:   propName,
		Switches: switches,
	}
	c.rwm.Unlock()
	c.write <- cmd

	var state PropertyState
	for {
		c.rwm.RLock()
		state = c.devices[deviceName].SwitchProperties[propName].State
		c.rwm.RUnlock()
		if state == PropertyStateOk {
			break
		}
		if state == PropertyStateAlert {
			return errors.New("unable to set switch property: " + prop.Name)
		}
	}

	return nil
}


// SetBlobValue sends a command to the INDI server to change the value of a blobVector.
func (c *INDIClient) SetBlobValue(deviceName, propName, blobName, blobValue, blobFormat string, blobSize int) error {
	c.rwm.Lock()
	device, err := c.findDevice(deviceName)
	if err != nil {
		c.rwm.Unlock()
		return err
	}

	prop, ok := device.BlobProperties[propName]
	if !ok {
		c.rwm.Unlock()
		return ErrPropertyNotFound
	}

	if prop.State == PropertyStateBusy {
		c.rwm.Unlock()
		return ErrPropertyStateBusy
	}

	if prop.Permissions == PropertyPermissionReadOnly {
		c.rwm.Unlock()
		return ErrPropertyReadOnly
	}

	_, ok = prop.Values[blobName]
	if !ok {
		c.rwm.Unlock()
		return ErrPropertyValueNotFound
	}

	prop.State = PropertyStateBusy

	device.BlobProperties[propName] = prop

	c.devices[deviceName] = device
	
	cmd := NewBlobVector{
		Device: deviceName,
		Name:   propName,
		Blobs: []OneBlob{
			{
				Name:   blobName,
				Value:  blobValue,
				Size:   blobSize,
				Format: blobFormat,
			},
		},
	}

	c.rwm.Unlock()
	c.write <- cmd

	var state PropertyState
	for {
		c.rwm.RLock()
		state = c.devices[deviceName].BlobProperties[propName].State
		c.rwm.RUnlock()
		if state == PropertyStateOk {
			break
		}
		if state == PropertyStateAlert {
			return errors.New("unable to set blob property: " + prop.Name)
		}
	}

	return nil
}

// Reads INDIClient.devices. Only call when INDIClient.rwm is at least reader locked.
func (c *INDIClient) findDevice(name string) (Device, error) {
	if d, ok := c.devices[name]; ok {
		return d, nil
	}

	return Device{}, ErrDeviceNotFound
}

// Reads INDIClient.devices. Only call when INDIClient.rwm is at least reader locked.
func (c *INDIClient) findOrCreateDevice(name string) Device {
	device, err := c.findDevice(name)
	if err == ErrDeviceNotFound {
		device = Device{
			Name:             name,
			TextProperties:   map[string]TextProperty{},
			SwitchProperties: map[string]SwitchProperty{},
			NumberProperties: map[string]NumberProperty{},
			LightProperties:  map[string]LightProperty{},
			BlobProperties:   map[string]BlobProperty{},
		}
	}

	return device
}

type indiMessageHandler interface {
	defTextVector(item *DefTextVector)
	defSwitchVector(item *DefSwitchVector)
	defNumberVector(item *DefNumberVector)
	defLightVector(item *DefLightVector)
	defBlobVector(item *DefBlobVector)
	setSwitchVector(item *SetSwitchVector)
	setTextVector(item *SetTextVector)
	setNumberVector(item *SetNumberVector)
	setLightVector(item *SetLightVector)
	setBlobVector(item *SetBlobVector)
	message(item *Message)
	delProperty(item *DelProperty)
}


// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) defTextVector(item *DefTextVector) {
	device := c.findOrCreateDevice(item.Device)

	prop := TextProperty{
		Name:        item.Name,
		Label:       item.Label,
		Group:       item.Group,
		Permissions: item.Perm,
		State:       item.State,
		Values:      map[string]TextValue{},
		LastUpdated: time.Now(),
		Messages:    []MessageJSON{},
	}

	for _, val := range item.Texts {
		prop.Values[val.Name] = TextValue{
			Label: val.Label,
			Name:  val.Name,
			Value: strings.TrimSpace(val.Value),
		}
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.TextProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) defSwitchVector(item *DefSwitchVector) {
	device := c.findOrCreateDevice(item.Device)

	prop := SwitchProperty{
		Name:        item.Name,
		Label:       item.Label,
		Group:       item.Group,
		Permissions: item.Perm,
		Rule:        item.Rule,
		State:       item.State,
		Values:      map[string]SwitchValue{},
		LastUpdated: time.Now(),
		Messages:    []MessageJSON{},
	}

	for _, val := range item.Switches {
		prop.Values[val.Name] = SwitchValue{
			Label: val.Label,
			Name:  val.Name,
			Value: SwitchState(strings.TrimSpace(string(val.Value))),
		}
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.SwitchProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) defNumberVector(item *DefNumberVector) {
	device := c.findOrCreateDevice(item.Device)

	prop := NumberProperty{
		Name:        item.Name,
		Label:       item.Label,
		Group:       item.Group,
		Permissions: item.Perm,
		State:       item.State,
		Values:      map[string]NumberValue{},
		LastUpdated: time.Now(),
		Messages:    []MessageJSON{},
	}

	for _, val := range item.Numbers {
		prop.Values[val.Name] = NumberValue{
			Label:  val.Label,
			Name:   val.Name,
			Value:  strings.TrimSpace(val.Value),
			Format: val.Format,
			Min:    val.Min,
			Max:    val.Max,
			Step:   val.Step,
		}
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.NumberProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) defLightVector(item *DefLightVector) {
	device := c.findOrCreateDevice(item.Device)

	prop := LightProperty{
		Name:        item.Name,
		Label:       item.Label,
		Group:       item.Group,
		State:       item.State,
		Values:      map[string]LightValue{},
		LastUpdated: time.Now(),
		Messages:    []MessageJSON{},
	}

	for _, val := range item.Lights {
		prop.Values[val.Name] = LightValue{
			Label: val.Label,
			Name:  val.Name,
			Value: PropertyState(strings.TrimSpace(string(val.Value))),
		}
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.LightProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) defBlobVector(item *DefBlobVector) {
	device := c.findOrCreateDevice(item.Device)

	prop := BlobProperty{
		Name:        item.Name,
		Label:       item.Label,
		Group:       item.Group,
		State:       item.State,
		Values:      map[string]BlobValue{},
		LastUpdated: time.Now(),
		Messages:    []MessageJSON{},
	}

	for _, val := range item.Blobs {
		prop.Values[val.Name] = BlobValue{
			Label: val.Label,
			Name:  val.Name,
		}
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.BlobProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) setSwitchVector(item *SetSwitchVector) {
	device, err := c.findDevice(item.Device)
	if err != nil {
		c.log.WithField("device", item.Device).WithError(err).Warn("could not find device")
		return
	}

	var prop SwitchProperty
	if p, ok := device.SwitchProperties[item.Name]; ok {
		prop = p
	} else {
		c.log.WithField("device", item.Device).WithField("property", item.Name).Warn("could not find property")
		return
	}

	prop.State = item.State
	prop.Timeout = item.Timeout

	if len(item.Timestamp) == 0 {
		prop.LastUpdated = time.Now()
	} else {
		var err error
		prop.LastUpdated, err = time.ParseInLocation("2006-01-02T15:04:05.9", item.Timestamp, time.UTC)

		if err != nil {
			c.log.WithField("timestamp", item.Timestamp).WithError(err).Warn("error in time.ParseInLocation")
			prop.LastUpdated = time.Now()
		}
	}

	for _, val := range item.Switches {
		v, ok := prop.Values[val.Name]
		if !ok {
			continue
		}

		v.Value = SwitchState(strings.TrimSpace(string(val.Value)))

		prop.Values[val.Name] = v
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.SwitchProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) setTextVector(item *SetTextVector) {
	device, err := c.findDevice(item.Device)
	if err != nil {
		c.log.WithField("device", item.Device).WithError(err).Warn("could not find device")
		return
	}

	var prop TextProperty
	if p, ok := device.TextProperties[item.Name]; ok {
		prop = p
	} else {
		c.log.WithField("device", item.Device).WithField("property", item.Name).Warn("could not find property")
		return
	}

	prop.State = item.State
	prop.Timeout = item.Timeout

	if len(item.Timestamp) == 0 {
		prop.LastUpdated = time.Now()
	} else {
		var err error
		prop.LastUpdated, err = time.ParseInLocation("2006-01-02T15:04:05.9", item.Timestamp, time.UTC)

		if err != nil {
			c.log.WithField("timestamp", item.Timestamp).WithError(err).Warn("error in time.ParseInLocation")
			prop.LastUpdated = time.Now()
		}
	}

	for _, val := range item.Texts {
		v, ok := prop.Values[val.Name]
		if !ok {
			continue
		}

		v.Value = strings.TrimSpace(val.Value)

		prop.Values[val.Name] = v
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.TextProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) setNumberVector(item *SetNumberVector) {
	device, err := c.findDevice(item.Device)
	if err != nil {
		c.log.WithField("device", item.Device).WithError(err).Warn("could not find device")
		return
	}

	var prop NumberProperty
	if p, ok := device.NumberProperties[item.Name]; ok {
		prop = p
	} else {
		c.log.WithField("device", item.Device).WithField("property", item.Name).Warn("could not find property")
		return
	}

	prop.State = item.State
	prop.Timeout = item.Timeout

	if len(item.Timestamp) == 0 {
		prop.LastUpdated = time.Now()
	} else {
		var err error
		prop.LastUpdated, err = time.ParseInLocation("2006-01-02T15:04:05.9", item.Timestamp, time.UTC)

		if err != nil {
			c.log.WithField("timestamp", item.Timestamp).WithError(err).Warn("error in time.ParseInLocation")
			prop.LastUpdated = time.Now()
		}
	}

	for _, val := range item.Numbers {
		v, ok := prop.Values[val.Name]
		if !ok {
			continue
		}

		v.Value = strings.TrimSpace(val.Value)

		prop.Values[val.Name] = v
	}

	if len(item.Message) > 0 {
		fmt.Println(item.Message)
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.NumberProperties[item.Name] = prop

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) setLightVector(item *SetLightVector) {
	device, err := c.findDevice(item.Device)
	if err != nil {
		c.log.WithField("device", item.Device).WithError(err).Warn("could not find device")
		return
	}

	var prop LightProperty
	if p, ok := device.LightProperties[item.Name]; ok {
		prop = p
	} else {
		c.log.WithField("device", item.Device).WithField("property", item.Name).Warn("could not find property")
		return
	}

	prop.State = item.State

	if len(item.Timestamp) == 0 {
		prop.LastUpdated = time.Now()
	} else {
		var err error
		prop.LastUpdated, err = time.ParseInLocation("2006-01-02T15:04:05.9", item.Timestamp, time.UTC)

		if err != nil {
			c.log.WithField("timestamp", item.Timestamp).WithError(err).Warn("error in time.ParseInLocation")
			prop.LastUpdated = time.Now()
		}
	}

	for _, val := range item.Lights {
		v, ok := prop.Values[val.Name]
		if !ok {
			continue
		}

		v.Value = PropertyState(strings.TrimSpace(string(val.Value)))

		prop.Values[val.Name] = v
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.LightProperties[item.Name] = prop

	c.devices[item.Device] = device
}


// Modifies INDIClient.devices. Only call when INDIClient.rwm is locked.
func (c *INDIClient) setBlobVector(item *SetBlobVector) {
	device, err := c.findDevice(item.Device)
	if err != nil {
		c.log.WithField("device", item.Device).WithError(err).Warn("could not find device")
		return
	}

	var prop BlobProperty
	if p, ok := device.BlobProperties[item.Name]; ok {
		prop = p
	} else {
		c.log.WithField("device", item.Device).WithField("property", item.Name).Warn("could not find property")
		return
	}

	prop.State = item.State
	prop.Timeout = item.Timeout

	if len(item.Timestamp) == 0 {
		prop.LastUpdated = time.Now()
	} else {
		var err error
		prop.LastUpdated, err = time.ParseInLocation("2006-01-02T15:04:05.9", item.Timestamp, time.UTC)

		if err != nil {
			c.log.WithField("timestamp", item.Timestamp).WithError(err).Warn("error in time.ParseInLocation")
			prop.LastUpdated = time.Now()
		}
	}

	for _, val := range item.Blobs {
		v, ok := prop.Values[val.Name]
		if !ok {
			continue
		}

		fname := fmt.Sprintf("%s_%s_%s%s", item.Device, item.Name, val.Name, val.Format)

		f, err := c.fs.OpenFile(fname, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
		if err != nil {
			c.log.WithField("file", fname).WithError(err).Warn("error in c.fs.OpenFile")
			continue
		}

		var writers []io.Writer

		if ws, ok := c.blobStreams.Load(fmt.Sprintf("%s_%s_%s", item.Device, item.Name, val.Name)); ok {
			wss := ws.(map[string]io.Writer)

			for _, w := range wss {
				writers = append(writers, w)
			}
		}

		writers = append(writers, f)

		val.Value = strings.TrimSpace(val.Value)
		r := base64.NewDecoder(base64.StdEncoding, strings.NewReader(val.Value))

		dest := io.MultiWriter(writers...)

		written, err := io.Copy(dest, r)
		if err != nil {
			c.log.WithError(err).Warn("error in io.Copy")
			continue
		}

		v.Value = f.Name()
		v.Size = written

		f.Close()

		prop.Values[val.Name] = v
	}

	if len(item.Message) > 0 {
		prop.Messages = append(prop.Messages, MessageJSON{
			Message:   item.Message,
			Timestamp: time.Now(),
		})
	}

	device.BlobProperties[item.Name] = prop

	c.devices[item.Device] = device
}

func (c *INDIClient) message(item *Message) {
	device, err := c.findDevice(item.Device)
	if err != nil {
		c.log.WithField("device", item.Device).WithError(err).Warn("could not find device")
		return
	}

	device.Messages = append(device.Messages, MessageJSON{
		Message:   item.Message,
		Timestamp: time.Now(),
	})

	c.devices[item.Device] = device
}

// Modifies INDIClient.devices must only be called in locked environment
func (c *INDIClient) delProperty(item *DelProperty) {
	if len(item.Device) == 0 {
		for key, _ := range c.devices {
			delete(c.devices, key)
			return
		}
		return
	}

	if len(item.Name) == 0 {
		delete(c.devices, item.Device)
		return
	}

	device := c.findOrCreateDevice(item.Device)

	delete(device.TextProperties, item.Name)
	delete(device.NumberProperties, item.Name)
	delete(device.SwitchProperties, item.Name)
	delete(device.LightProperties, item.Name)
	delete(device.BlobProperties, item.Name)

	c.devices[item.Device] = device
}

func (c *INDIClient) startRead() {
	go func(r <-chan interface{}, log logging.Logger, lock *sync.RWMutex, handler indiMessageHandler) {
		for i := range r {
			log.WithField("item", i).Debug("got message")

			lock.Lock()
			switch item := i.(type) {
			case *DefTextVector:
				handler.defTextVector(item)
			case *DefSwitchVector:
				handler.defSwitchVector(item)
			case *DefNumberVector:
				handler.defNumberVector(item)
			case *DefLightVector:
				handler.defLightVector(item)
			case *DefBlobVector:
				handler.defBlobVector(item)
			case *SetSwitchVector:
				handler.setSwitchVector(item)
			case *SetTextVector:
				handler.setTextVector(item)
			case *SetNumberVector:
				handler.setNumberVector(item)
			case *SetLightVector:
				handler.setLightVector(item)
			case *SetBlobVector:
				handler.setBlobVector(item)
			case *Message:
				handler.message(item)
			case *DelProperty:
				handler.delProperty(item)
			default:
				log.WithField("type", fmt.Sprintf("%T", item)).Warn("unknown type")
			}
			lock.Unlock()
		}
	}(c.read, c.log, c.rwm, c)

	go func(conn io.Reader, r chan<- interface{}, log logging.Logger) {
		decoder := xml.NewDecoder(conn)

		var inElement string
		for {
			t, err := decoder.Token()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					// We've disconnected.
					return
				}

				log.WithError(err).Warn("error in decoder.Token")

				if err == io.EOF {
					c.Disconnect()
					return
				}
				continue
			}

			var item interface{}

			switch se := t.(type) {
			case xml.StartElement:
				log.WithField("startElement", se.Name.Local).Debug("read start element")

				var inner interface{}
				inElement = se.Name.Local
				switch inElement {
				case "defSwitchVector":
					inner = &DefSwitchVector{}
				case "defTextVector":
					inner = &DefTextVector{}
				case "defNumberVector":
					inner = &DefNumberVector{}
				case "defLightVector":
					inner = &DefLightVector{}
				case "defBLOBVector":
					inner = &DefBlobVector{}
				case "setSwitchVector":
					inner = &SetSwitchVector{}
				case "setTextVector":
					inner = &SetTextVector{}
				case "setNumberVector":
					inner = &SetNumberVector{}
				case "setLightVector":
					inner = &SetLightVector{}
				case "setBLOBVector":
					inner = &SetBlobVector{}
				case "message":
					inner = &Message{}
				case "delProperty":
					inner = &DelProperty{}
				default:
					log.WithField("element", inElement).Error("unknown element")
				}

				if inner != nil {
					err = decoder.DecodeElement(&inner, &se)
					if err != nil {
						log.WithField("element", inElement).WithError(err).Error("error in decoder.DecodeElement")
						continue
					}

					item = inner
				}
			}

			if item != nil {
				r <- item
			}
		}
	}(c.conn, c.read, c.log)
}

func (c *INDIClient) startWrite() {
	go func(conn io.Writer, w chan interface{}, log logging.Logger, lock *sync.RWMutex, handler indiMessageHandler) {
		for item := range w {
			lock.Lock()
			b, err := xml.Marshal(item)
			if err != nil {
				log.WithError(err).Error("error in xml.Marshal")
				lock.Unlock()
				continue
			}

			log.WithField("cmd", string(b)).Debug("sending command")
			_, err = conn.Write(b)
			if err != nil {
				log.WithError(err).Error("error in conn.Write")
				lock.Unlock()
				continue
			}
			lock.Unlock()
		}
	}(c.conn, c.write, c.log, c.rwm, c)
}
