package installationproxy

import (
	"bytes"
	"fmt"

	log "github.com/sirupsen/logrus"

	ios "github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"
)

const serviceName = "com.apple.mobile.installation_proxy"

const (
	// ApplicationType shows if this is a 'User', 'System' or 'Hidden' app
	ApplicationType            = "ApplicationType"
	CFBundleDisplayName        = "CFBundleDisplayName"
	CFBundleExecutable         = "CFBundleExecutable"
	CFBundleIdentifier         = "CFBundleIdentifier"
	CFBundleName               = "CFBundleName"
	CFBundleNumericVersion     = "CFBundleNumericVersion"
	CFBundleShortVersionString = "CFBundleShortVersionString"
	CFBundleSupportedPlatforms = "CFBundleSupportedPlatforms"
	CFBundleVersion            = "CFBundleVersion"
	// DTXcode is the Xcode version the app was built with (e.g. Xcode 16.4 is '1640')
	DTXcode = "DTXcode"
	// DTXcodeBuild is the Xcode build version the app was built with
	DTXcodeBuild         = "DTXcodeBuild"
	Entitlements         = "Entitlements"
	EnvironmentVariables = "EnvironmentVariables"
	// MinimumOSVersion defines the minimum supported iOS version
	MinimumOSVersion = "MinimumOSVersion"
	Path             = "Path"
	// UIDeviceFamily slice of integers for supported devices types where a value of '1' means iPhone, and '2' iPad
	UIDeviceFamily       = "UIDeviceFamily"
	UIFileSharingEnabled = "UIFileSharingEnabled"
)

type Connection struct {
	deviceConn ios.DeviceConnectionInterface
	plistCodec ios.PlistCodec
}

func (c *Connection) Close() {
	c.deviceConn.Close()
}

func New(device ios.DeviceEntry) (*Connection, error) {
	deviceConn, err := ios.ConnectToService(device, serviceName)
	if err != nil {
		return &Connection{}, err
	}
	return &Connection{deviceConn: deviceConn, plistCodec: ios.NewPlistCodec()}, nil
}

// NewFromConn creates a Connection from an existing DeviceConnectionInterface.
// This allows callers to establish the connection externally (e.g. via shared lockdown session).
func NewFromConn(conn ios.DeviceConnectionInterface) *Connection {
	return &Connection{deviceConn: conn, plistCodec: ios.NewPlistCodec()}
}

func (conn *Connection) BrowseUserApps() ([]AppInfo, error) {
	return conn.browseApps(browseApps("User", true))
}

func (conn *Connection) BrowseSystemApps() ([]AppInfo, error) {
	return conn.browseApps(browseApps("System", false))
}

func (conn *Connection) BrowseFileSharingApps() ([]AppInfo, error) {
	return conn.browseApps(browseApps("Filesharing", true))
}

func (conn *Connection) BrowseAllApps() ([]AppInfo, error) {
	return conn.browseApps(browseApps("", true))
}

func (conn *Connection) browseApps(request interface{}) ([]AppInfo, error) {
	reader := conn.deviceConn.Reader()
	bytes, err := conn.plistCodec.Encode(request)
	if err != nil {
		return make([]AppInfo, 0), err
	}
	conn.deviceConn.Send(bytes)
	stillReceiving := true
	responses := make([]BrowseResponse, 0)
	size := uint64(0)
	for stillReceiving {
		response, err := conn.plistCodec.Decode(reader)
		ifa, err := plistFromBytes(response)
		stillReceiving = "Complete" != ifa.Status
		if err != nil {
			return make([]AppInfo, 0), err
		}
		size += ifa.CurrentAmount
		responses = append(responses, ifa)
	}
	appinfos := make([]AppInfo, size)

	for _, v := range responses {
		copy(appinfos[v.CurrentIndex:], v.CurrentList)
	}
	return appinfos, nil
}

func (c *Connection) Uninstall(bundleId string) error {
	options := map[string]interface{}{}
	uninstallCommand := map[string]interface{}{
		"Command":               "Uninstall",
		"ApplicationIdentifier": bundleId,
		"ClientOptions":         options,
	}
	return c.sendCommandAndWait(uninstallCommand)
}

// Install sends an Install command to installationproxy for a package that has
// already been staged on the device (e.g. via AFC to PublicStaging/).
// packagePath is the device-local path such as "PublicStaging/bundleID".
// bundleID is optional; if non-empty, it is passed as CFBundleIdentifier in ClientOptions.
func (c *Connection) Install(packagePath string, bundleID string) error {
	opts := map[string]interface{}{}
	if bundleID != "" {
		opts["CFBundleIdentifier"] = bundleID
	}
	installCommand := map[string]interface{}{
		"Command":       "Install",
		"PackagePath":   packagePath,
		"ClientOptions": opts,
	}
	return c.sendCommandAndWait(installCommand)
}

func (c *Connection) sendCommandAndWait(command map[string]interface{}) error {
	b, err := c.plistCodec.Encode(command)
	if err != nil {
		return err
	}
	err = c.deviceConn.Send(b)
	if err != nil {
		return err
	}
	for {
		response, err := c.plistCodec.Decode(c.deviceConn.Reader())
		if err != nil {
			return err
		}
		dict, err := ios.ParsePlist(response)
		if err != nil {
			return err
		}
		done, err := checkFinished(dict)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func checkFinished(dict map[string]interface{}) (bool, error) {
	if val, ok := dict["Error"]; ok {
		errMsg := fmt.Sprintf("%v", val)
		if desc, ok := dict["ErrorDescription"]; ok {
			errMsg = fmt.Sprintf("%s: %v", errMsg, desc)
		}
		if detail, ok := dict["ErrorDetail"]; ok {
			errMsg = fmt.Sprintf("%s (error code: %v)", errMsg, detail)
		}
		return true, fmt.Errorf("install failed: %s", errMsg)
	}
	if val, ok := dict["Status"]; ok {
		if "Complete" == val {
			log.Info("operation completed successfully")
			return true, nil
		}
		percent, _ := dict["PercentComplete"]
		log.WithFields(log.Fields{"status": val, "percentComplete": percent}).Info("installing")
		return false, nil
	}
	return true, fmt.Errorf("unknown status update: %+v", dict)
}

func plistFromBytes(plistBytes []byte) (BrowseResponse, error) {
	var browseResponse BrowseResponse
	decoder := plist.NewDecoder(bytes.NewReader(plistBytes))

	err := decoder.Decode(&browseResponse)
	if err != nil {
		return browseResponse, err
	}
	return browseResponse, nil
}

func browseApps(applicationType string, showLaunchProhibitedApps bool) map[string]interface{} {
	clientOptions := map[string]any{}
	if applicationType != "" && applicationType != "Filesharing" {
		clientOptions["ApplicationType"] = applicationType
	}
	if showLaunchProhibitedApps {
		clientOptions["ShowLaunchProhibitedApps"] = true
	}
	return map[string]interface{}{"ClientOptions": clientOptions, "Command": "Browse"}
}

type BrowseResponse struct {
	CurrentIndex  uint64
	CurrentAmount uint64
	Status        string
	CurrentList   []AppInfo
}
type AppInfo map[string]any

func (a AppInfo) CFBundleIdentifier() string {
	if bundleId, ok := a[CFBundleIdentifier].(string); ok {
		return bundleId
	}
	return ""
}

func (a AppInfo) Path() string {
	if path, ok := a[Path].(string); ok {
		return path
	}
	return ""
}

func (a AppInfo) CFBundleName() string {
	if bundleName, ok := a[CFBundleName].(string); ok {
		return bundleName
	}
	return ""
}

func (a AppInfo) EnvironmentVariables() map[string]any {
	if envVars, ok := a[EnvironmentVariables].(map[string]any); ok {
		return envVars
	}
	return make(map[string]any)
}

func (a AppInfo) CFBundleExecutable() string {
	if executable, ok := a[CFBundleExecutable].(string); ok {
		return executable
	}
	return ""
}

func (a AppInfo) CFBundleShortVersionString() string {
	if shortVersion, ok := a[CFBundleShortVersionString].(string); ok {
		return shortVersion
	}
	return ""
}

func (a AppInfo) UIFileSharingEnabled() bool {
	if fileSharingEnabled, ok := a[UIFileSharingEnabled].(bool); ok {
		return fileSharingEnabled
	}
	return false
}
