package nutanix

import (
	"strings"
	"time"

	"fmt"
	"log"
	"path"

	client "github.com/nutanix-cloud-native/prism-go-client/pkg/nutanix"
	v3 "github.com/nutanix-cloud-native/prism-go-client/pkg/nutanix/v3"
)

const (
	defaultImageBuiltDescription = "built by Packer"
	defaultImageDLDescription    = "added by Packer"
	vmDescription                = "Packer vm building image %s"
)

// A driver is able to talk to Nutanix PrismCentral and perform certain
// operations with it.
type Driver interface {
	CreateRequest(VmConfig) (*v3.VMIntentInput, error)
	Create(*v3.VMIntentInput) (*nutanixInstance, error)
	Delete(string) error
	GetVM(string) (*nutanixInstance, error)
	//GetImage(string) (*nutanixImage, error)
	GetHost(string) (*nutanixHost, error)
	PowerOff(string) error
	UploadImage(string, string, string, VmConfig) (*nutanixImage, error)
	DeleteImage(string) error
	GetImage(string) (*nutanixImage, error)
	SaveVMDisk(string, int, []Category) (*nutanixImage, error)
	WaitForShutdown(string, <-chan struct{}) bool
}

type NutanixDriver struct {
	Config        Config
	ClusterConfig ClusterConfig
	vmEndCh       <-chan int
}

type nutanixInstance struct {
	nutanix v3.VMIntentResponse
}

type nutanixHost struct {
	host v3.HostResponse
}

type nutanixImage struct {
	image v3.ImageIntentResponse
}

func findClusterByName(conn *v3.Client, name string) (*v3.ClusterIntentResponse, error) {
	filter := fmt.Sprintf("name==%s", name)
	resp, err := conn.V3.ListAllCluster(filter)
	if err != nil {
		return nil, err
	}
	entities := resp.Entities

	found := make([]*v3.ClusterIntentResponse, 0)
	for _, v := range entities {
		if *v.Status.Name == name {
			found = append(found, &v3.ClusterIntentResponse{
				Status:     v.Status,
				Spec:       v.Spec,
				Metadata:   v.Metadata,
				APIVersion: v.APIVersion,
			})
		}
	}

	if len(found) > 1 {
		return nil, fmt.Errorf("your query returned more than one result. Please use cluster_uuid argument instead")
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("did not find cluster with name %s", name)
	}

	return found[0], nil
}

func findSubnetByUUID(conn *v3.Client, uuid string) (*v3.SubnetIntentResponse, error) {
	return conn.V3.GetSubnet(uuid)
}

func findSubnetByName(conn *v3.Client, name string) (*v3.SubnetIntentResponse, error) {
	filter := fmt.Sprintf("name==%s", name)
	resp, err := conn.V3.ListAllSubnet(filter, getEmptyClientSideFilter())
	if err != nil {
		return nil, err
	}

	entities := resp.Entities

	found := make([]*v3.SubnetIntentResponse, 0)
	for _, v := range entities {
		if *v.Spec.Name == name {
			found = append(found, v)
		}
	}

	if len(found) > 1 {
		return nil, fmt.Errorf("your query returned more than one result. Please use subnet_uuid argument or use additional filters instead")
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("subnet with the given name, not found")
	}

	return found[0], nil
}
func sourceImageExists(conn *v3.Client, name string, uri string) (*v3.ImageIntentResponse, error) {
	filter := fmt.Sprintf("name==%s", name)
	resp, err := conn.V3.ListAllImage(filter)
	if err != nil {
		return nil, err
	}

	entities := resp.Entities

	found := make([]*v3.ImageIntentResponse, 0)
	for _, v := range entities {
		if (*v.Spec.Name == name) && (*v.Status.Resources.SourceURI == uri) {
			found = append(found, v)
		}
	}

	if len(found) > 1 {
		return nil, fmt.Errorf("your query returned more than one result with same Name/URI")
	}

	if len(found) == 0 {
		return nil, nil
	}
	return found[0], nil
}

func findImageByUUID(conn *v3.Client, uuid string) (*v3.ImageIntentResponse, error) {
	return conn.V3.GetImage(uuid)
}

func findImageByName(conn *v3.Client, name string) (*v3.ImageIntentResponse, error) {
	filter := fmt.Sprintf("name==%s", name)
	resp, err := conn.V3.ListAllImage(filter)
	if err != nil {
		return nil, err
	}

	entities := resp.Entities

	found := make([]*v3.ImageIntentResponse, 0)
	for _, v := range entities {
		if *v.Spec.Name == name {
			found = append(found, v)
		}
	}

	if len(found) > 1 {
		return nil, fmt.Errorf("your query returned multiple results with name %s. Please use soure_image_uuid argument instead", name)
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("image %s not found", name)
	}

	return findImageByUUID(conn, *found[0].Metadata.UUID)
}

func checkTask(conn *v3.Client, taskUUID string) error {

	log.Printf("checking task %s...", taskUUID)
	var task *v3.TasksResponse
	var err error
	for i := 0; i < 120; i++ {
		task, err = conn.V3.GetTask(taskUUID)
		if err == nil {
			if *task.Status == "SUCCEEDED" {
				return nil
			} else if *task.Status == "FAILED" {
				return fmt.Errorf(*task.ErrorDetail)
			} else {
				log.Printf("task status is " + *task.Status)
				<-time.After(5 * time.Second)
			}
		} else {
			return err
		}
	}
	return fmt.Errorf("check task %s timeout", taskUUID)
}

func (d *nutanixInstance) Addresses() []string {
	var addresses []string
	if len(d.nutanix.Status.Resources.NicList) > 0 {
		for _, n := range d.nutanix.Status.Resources.NicList {
			addresses = append(addresses, *n.IPEndpointList[0].IP)
		}
	}
	return addresses
}

func (d *nutanixInstance) PowerState() string {
	return *d.nutanix.Status.Resources.PowerState
}

func (d *NutanixDriver) WaitForShutdown(vmUUID string, cancelCh <-chan struct{}) bool {
	endCh := d.vmEndCh

	if endCh == nil {
		return true
	}

	select {
	case <-endCh:
		return true
	case <-cancelCh:
		return false
	}
}
func (d *NutanixDriver) CreateRequest(vm VmConfig) (*v3.VMIntentInput, error) {

	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, err
	}

	// If UserData exists, create GuestCustomization
	var guestCustomization *v3.GuestCustomization
	if vm.UserData == "" {
		guestCustomization = nil
	} else {
		if vm.OSType == "Windows" {
			installType := "FRESH"
			guestCustomization = &v3.GuestCustomization{
				Sysprep: &v3.GuestCustomizationSysprep{
					InstallType: &installType,
					UnattendXML: &vm.UserData,
				},
			}
		}
		if vm.OSType == "Linux" {
			guestCustomization = &v3.GuestCustomization{
				CloudInit: &v3.GuestCustomizationCloudInit{
					UserData: &vm.UserData,
				},
			}
		}
	}
	DiskList := []*v3.VMDisk{}
	SATAindex := 0
	SCSIindex := 0
	for _, disk := range vm.VmDisks {
		if disk.ImageType == "DISK_IMAGE" {
			image := &v3.ImageIntentResponse{}
			if disk.SourceImageURI != "" {
				image, err := d.UploadImage(disk.SourceImageURI, "URI", disk.ImageType, vm)
				if err != nil {
					return nil, fmt.Errorf("error while findImageByUUID, Error %s", err.Error())
				}
				disk.SourceImageUUID = *image.image.Metadata.UUID
			}
			if disk.SourceImageUUID != "" {
				image, err = findImageByUUID(conn, disk.SourceImageUUID)
				if err != nil {
					return nil, fmt.Errorf("error while findImageByUUID, Error %s", err.Error())
				}
			} else if disk.SourceImageName != "" {
				image, err = findImageByName(conn, disk.SourceImageName)
				if err != nil {
					return nil, fmt.Errorf("error while findImageByName, %s", err.Error())
				}
			}

			DeviceType := "DISK"
			AdapterType := "SCSI"
			DeviceIndex := int64(SCSIindex)
			DiskSizeMib := disk.DiskSizeGB * 1024
			if disk.DiskSizeGB == 0 {
				DiskSizeMib = *image.Status.Resources.SizeBytes / 1024 / 1024
			}
			newDisk := v3.VMDisk{
				DeviceProperties: &v3.VMDiskDeviceProperties{
					DeviceType: &DeviceType,
					DiskAddress: &v3.DiskAddress{
						AdapterType: &AdapterType,
						DeviceIndex: &DeviceIndex,
					},
				},
				DataSourceReference: BuildReference(*image.Metadata.UUID, "image"),
				DiskSizeMib:         &DiskSizeMib,
			}
			DiskList = append(DiskList, &newDisk)
			SCSIindex++
		}
		if disk.ImageType == "DISK" {
			DeviceType := "DISK"
			AdapterType := "SCSI"
			DeviceIndex := int64(SCSIindex)
			DiskSizeMib := disk.DiskSizeGB * 1024
			newDisk := v3.VMDisk{
				DeviceProperties: &v3.VMDiskDeviceProperties{
					DeviceType: &DeviceType,
					DiskAddress: &v3.DiskAddress{
						AdapterType: &AdapterType,
						DeviceIndex: &DeviceIndex,
					},
				},
				DiskSizeMib: &DiskSizeMib,
			}
			DiskList = append(DiskList, &newDisk)
			SCSIindex++
		}
		if disk.ImageType == "ISO_IMAGE" {
			image := &v3.ImageIntentResponse{}
			if disk.SourceImageURI != "" {
				image, err := d.UploadImage(disk.SourceImageURI, "URI", disk.ImageType, vm)
				if err != nil {
					return nil, fmt.Errorf("error while findImageByUUID, Error %s", err.Error())
				}
				disk.SourceImageUUID = *image.image.Metadata.UUID
			}
			if disk.SourceImageUUID != "" {
				image, err = findImageByUUID(conn, disk.SourceImageUUID)
				if err != nil {
					return nil, fmt.Errorf("error while findImageByUUID, %s", err.Error())
				}
			} else if disk.SourceImageName != "" {
				image, err = findImageByName(conn, disk.SourceImageName)
				if err != nil {
					return nil, fmt.Errorf("error while findImageByName, %s", err.Error())
				}
			}
			DeviceType := "CDROM"
			AdapterType := "SATA"
			DeviceIndex := int64(SATAindex)
			newDisk := v3.VMDisk{
				DeviceProperties: &v3.VMDiskDeviceProperties{
					DeviceType: &DeviceType,
					DiskAddress: &v3.DiskAddress{
						AdapterType: &AdapterType,
						DeviceIndex: &DeviceIndex,
					},
				},
				DataSourceReference: BuildReference(*image.Metadata.UUID, "image"),
			}
			DiskList = append(DiskList, &newDisk)
			SATAindex++
		}
	}

	NICList := []*v3.VMNic{}
	for _, nic := range vm.VmNICs {
		subnet := &v3.SubnetIntentResponse{}
		if nic.SubnetUUID != "" {
			subnet, err = findSubnetByUUID(conn, nic.SubnetUUID)
			if err != nil {
				return nil, fmt.Errorf("error while findSubnetByUUID, %s", err.Error())
			}
		} else if nic.SubnetName != "" {
			subnet, err = findSubnetByName(conn, nic.SubnetName)
			if err != nil {
				return nil, fmt.Errorf("error while findSubnetByName, %s", err.Error())
			}
		}

		isConnected := true
		newNIC := v3.VMNic{
			IsConnected:     &isConnected,
			SubnetReference: BuildReference(*subnet.Metadata.UUID, "subnet"),
		}
		NICList = append(NICList, &newNIC)
	}
	PowerStateOn := "ON"

	cluster := &v3.ClusterIntentResponse{}
	if vm.ClusterUUID != "" {
		cluster, err = conn.V3.GetCluster(vm.ClusterUUID)
		if err != nil {
			return nil, fmt.Errorf("error while GetCluster, %s", err.Error())
		}
	} else if vm.ClusterName != "" {
		cluster, err = findClusterByName(conn, vm.ClusterName)
		if err != nil {
			return nil, fmt.Errorf("error while findClusterByName, %s", err.Error())
		}
	}

	req := &v3.VMIntentInput{
		Spec: &v3.VM{
			Name: &vm.VMName,
			Resources: &v3.VMResources{
				GuestCustomization: guestCustomization,
				NumSockets:         &vm.CPU,
				MemorySizeMib:      &vm.MemoryMB,
				PowerState:         &PowerStateOn,
				DiskList:           DiskList,
				NicList:            NICList,
			},
			ClusterReference: BuildReference(*cluster.Metadata.UUID, "cluster"),
			Description:      StringPtr(fmt.Sprintf(vmDescription, d.Config.VmConfig.ImageName)),
		},
		Metadata: &v3.Metadata{
			Kind: StringPtr("vm"),
		},
	}

	if vm.BootType == NutanixIdentifierBootTypeUEFI {
		bootType := strings.ToUpper(vm.BootType)

		req.Spec.Resources.BootConfig = &v3.VMBootConfig{
			BootType: &bootType,
		}
	}

	if len(vm.VMCategories) != 0 {
		c := make(map[string]string)
		for _, category := range vm.VMCategories {
			c[category.Key] = category.Value
		}
		req.Metadata.Categories = c
	}

	return req, nil

}

func (d *NutanixDriver) Create(req *v3.VMIntentInput) (*nutanixInstance, error) {

	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, err
	}

	log.Printf("creating vm %s...", d.Config.VMName)

	resp, err := conn.V3.CreateVM(req)
	if err != nil {
		log.Printf("error creating vm: %s", err.Error())
		return nil, err
	}

	uuid := *resp.Metadata.UUID

	err = checkTask(conn, resp.Status.ExecutionContext.TaskUUID.(string))

	if err != nil {
		log.Printf("error creating vm: %s", err.Error())
		return nil, err
	}

	log.Print("vm succesfully created")

	// Wait for the VM obtain an IP address

	log.Printf("[INFO] Waiting for IP, up to timeout: %s", d.Config.WaitTimeout)

	iteration := int(d.Config.WaitTimeout.Seconds()) / 5
	var vm *v3.VMIntentResponse
	for i := 0; i < iteration; i++ {
		vm, err = conn.V3.GetVM(uuid)
		if err != nil || len(vm.Status.Resources.NicList[0].IPEndpointList) == (0) {
			log.Printf("Waiting VM (%s) ip configuration", uuid)
			<-time.After(5 * time.Second)
			continue
		}
		IPAddress := *vm.Status.Resources.NicList[0].IPEndpointList[0].IP
		log.Printf("VM (%s) configured with ip address %s", uuid, IPAddress)
		return &nutanixInstance{nutanix: *vm}, err
	}
	return nil, fmt.Errorf("not able to get ip address for vm (%s)", uuid)
}

func (d *NutanixDriver) Delete(vmUUID string) error {

	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return err
	}

	_, err = conn.V3.DeleteVM(vmUUID)
	if err != nil {
		return err
	}
	return nil
}

// UploadImage (string, VmConfig) (*nutanixImage, error)
func (d *NutanixDriver) UploadImage(imagePath string, sourceType string, imageType string, vm VmConfig) (*nutanixImage, error) {
	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, err
	}

	_, file := path.Split(imagePath)

	cluster := &v3.ClusterIntentResponse{}
	if vm.ClusterUUID != "" {
		cluster, err = conn.V3.GetCluster(vm.ClusterUUID)
		if err != nil {
			return nil, fmt.Errorf("error while GetCluster, %s", err.Error())
		}
	} else if vm.ClusterName != "" {
		cluster, err = findClusterByName(conn, vm.ClusterName)
		if err != nil {
			return nil, fmt.Errorf("error while findClusterByName, %s", err.Error())
		}
	}

	refvalue := BuildReferenceValue(*cluster.Metadata.UUID, "cluster")
	InitialPlacementRef := []*v3.ReferenceValues{refvalue}
	req := &v3.ImageIntentInput{
		Spec: &v3.Image{
			Name: &file,
			Resources: &v3.ImageResources{
				ImageType:               &imageType,
				InitialPlacementRefList: InitialPlacementRef,
			},
			Description: StringPtr(defaultImageDLDescription),
		},
		Metadata: &v3.Metadata{
			Kind: StringPtr("image"),
		},
	}
	if sourceType == "URI" {
		image, err := sourceImageExists(conn, file, imagePath)
		if err != nil {
			return nil, fmt.Errorf("error while checking if image exists, %s", err.Error())
		}
		if image != nil {
			return &nutanixImage{image: *image}, nil
		}
		req.Spec.Resources.SourceURI = &imagePath
	}

	log.Printf("creating image: %s", file)
	image, err := conn.V3.CreateImage(req)
	if err != nil {
		return nil, fmt.Errorf("error while create image: %s", err.Error())
	}

	err = checkTask(conn, image.Status.ExecutionContext.TaskUUID.(string))
	if err != nil {
		return nil, fmt.Errorf("error while create image: %s", err.Error())
	}

	if sourceType == "PATH" {
		log.Printf("uploading image: %s", imagePath)
		err = conn.V3.UploadImage(*image.Metadata.UUID, imagePath)
		if err != nil {
			return nil, fmt.Errorf("error while upload image: %s", err.Error())
		}

		running, err := conn.V3.GetImage(*image.Metadata.UUID)
		if err != nil || *running.Status.State != "COMPLETE" {
			return nil, fmt.Errorf("error while upload image: %s", err.Error())
		}
	}
	return &nutanixImage{image: *image}, nil

}

func (d *NutanixDriver) DeleteImage(imageUUID string) error {
	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return fmt.Errorf("error while creating new client connection, %s", err.Error())
	}
	_, err = conn.V3.DeleteImage(imageUUID)
	if err != nil {
		return fmt.Errorf("error while deleting image, %s", err.Error())
	}
	return nil
}

func (d *NutanixDriver) GetImage(imagename string) (*nutanixImage, error) {
	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, fmt.Errorf("error while NewV3Client, %s", err.Error())
	}

	image, err := findImageByName(conn, imagename)
	if err != nil {
		return nil, fmt.Errorf("error while GetImage, %s", err.Error())
	}
	return &nutanixImage{image: *image}, nil
}

func (d *NutanixDriver) GetVM(vmUUID string) (*nutanixInstance, error) {

	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, fmt.Errorf("error while NewV3Client, %s", err.Error())
	}

	vm, err := conn.V3.GetVM(vmUUID)
	if err != nil {
		return nil, fmt.Errorf("error while GetVM, %s", err.Error())
	}
	return &nutanixInstance{nutanix: *vm}, nil
}

func (d *NutanixDriver) GetHost(hostUUID string) (*nutanixHost, error) {

	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, fmt.Errorf("error while NewV3Client, %s", err.Error())
	}

	host, err := conn.V3.GetHost(hostUUID)
	if err != nil {
		return nil, fmt.Errorf("error while GetHost, %s", err.Error())
	}
	return &nutanixHost{host: *host}, nil
}

func (d *NutanixDriver) PowerOff(vmUUID string) error {
	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return fmt.Errorf("error while NewV3Client, %s", err.Error())
	}
	vmResp, err := conn.V3.GetVM(vmUUID)
	if err != nil {
		return fmt.Errorf("error while GetVM, %s", err.Error())
	}

	// Prepare VM update request
	request := &v3.VMIntentInput{}
	request.Spec = vmResp.Spec
	request.Metadata = vmResp.Metadata
	request.Spec.Resources.PowerState = StringPtr("OFF")

	resp, err := conn.V3.UpdateVM(vmUUID, request)
	if err != nil {
		return fmt.Errorf("error while UpdateVM, %s", err.Error())
	}

	taskUUID := resp.Status.ExecutionContext.TaskUUID.(string)

	// Wait for the VM to be deleted
	for i := 0; i < 1200; i++ {
		resp, err := conn.V3.GetTask(taskUUID)
		if err != nil || *resp.Status != "SUCCEEDED" {
			<-time.After(1 * time.Second)
			continue
		}
		return fmt.Errorf("error while GetTask, %s", err.Error())
	}

	log.Printf("PowerOff task: %s", taskUUID)
	return nil
}
func (d *NutanixDriver) SaveVMDisk(diskUUID string, index int, imageCategories []Category) (*nutanixImage, error) {

	configCreds := client.Credentials{
		URL:      fmt.Sprintf("%s:%d", d.ClusterConfig.Endpoint, d.ClusterConfig.Port),
		Endpoint: d.ClusterConfig.Endpoint,
		Username: d.ClusterConfig.Username,
		Password: d.ClusterConfig.Password,
		Port:     string(d.ClusterConfig.Port),
		Insecure: d.ClusterConfig.Insecure,
	}

	conn, err := v3.NewV3Client(configCreds)
	if err != nil {
		return nil, fmt.Errorf("error while NewV3Client, %s", err.Error())
	}

	name := d.Config.VmConfig.ImageName
	if index > 0 {
		name = fmt.Sprintf("%s-disk%d", name, index+1)
	}

	// When force_deregister, check if image already exists
	if d.Config.ForceDeregister {
		log.Println("force_deregister is set, check if image already exists")
		ImageList, err := conn.V3.ListAllImage(fmt.Sprintf("name==%s", name))
		if err != nil {
			return nil, fmt.Errorf("error while ListAllImage, %s", err.Error())
		}
		if *ImageList.Metadata.TotalMatches == 0 {
			log.Println("image with given Name not found, no need to deregister")
		} else if *ImageList.Metadata.TotalMatches > 1 {
			log.Println("more than one image with given Name found, will not deregister")
		} else if *ImageList.Metadata.TotalMatches == 1 {
			log.Println("exactly one image with given Name found, will deregister")

			resp, err := conn.V3.DeleteImage(*ImageList.Entities[0].Metadata.UUID)
			if err != nil {
				return nil, fmt.Errorf("error while Delete Image, %s", err.Error())
			}

			log.Printf("deleting image %s...\n", *ImageList.Entities[0].Metadata.UUID)
			err = checkTask(conn, resp.Status.ExecutionContext.TaskUUID.(string))

			if err != nil {
				return nil, fmt.Errorf("error while Delete Image, %s", err.Error())
			}
		}
	}

	imgDescription := defaultImageBuiltDescription
	if d.Config.ImageDescription != "" {
		imgDescription = d.Config.ImageDescription
	}

	req := &v3.ImageIntentInput{
		Spec: &v3.Image{
			Name: &name,
			Resources: &v3.ImageResources{
				ImageType:           StringPtr("DISK_IMAGE"),
				DataSourceReference: BuildReference(diskUUID, "vm_disk"),
			},
			Description: StringPtr(imgDescription),
		},
		Metadata: &v3.Metadata{
			Kind: StringPtr("image"),
		},
	}

	if len(imageCategories) != 0 {
		c := make(map[string]string)
		for _, category := range imageCategories {
			c[category.Key] = category.Value
		}
		req.Metadata.Categories = c
	}

	image, err := conn.V3.CreateImage(req)
	if err != nil {
		return nil, fmt.Errorf("error while Create Image, %s", err.Error())
	}
	log.Printf("creating image %s...\n", *image.Metadata.UUID)
	err = checkTask(conn, image.Status.ExecutionContext.TaskUUID.(string))
	if err != nil {
		return nil, fmt.Errorf("error while Create Image, %s", err.Error())
	} else {
		return &nutanixImage{image: *image}, nil
	}

}

func getEmptyClientSideFilter() []*client.AdditionalFilter {
	return make([]*client.AdditionalFilter, 0)
}
