package main

import (
	"strconv"
	"sync"

	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"

	"github.com/jacobsa/fuse"

	"path/filepath"

	goofys "github.com/haibin-fx/goofys-docker/internal"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/docker/go-plugins-helpers/volume"
	_ "golang.org/x/net/context"
	"github.com/jacobsa/fuse/fuseutil"
)

type s3Driver struct {
	root        string
	connections map[string]int
	volumes     map[string]map[string]string
	m           *sync.Mutex
}
const (
	socketAddress = "/run/docker/plugins/goofys.sock"
)

var (
	defaultPath = filepath.Join("/mnt/", "volumes")
	root        = flag.String("root", defaultPath, "Docker volumes root directory")
)

func main() {
	flag.Parse()

	d := newS3Driver(*root)
	h := volume.NewHandler(d)

	fmt.Printf("Listening on %s\n", socketAddress)
	fmt.Println(h.ServeUnix(socketAddress, syscall.Getgid()))
}
func newS3Driver(root string) s3Driver {
	return s3Driver{
		root:        root,
		connections: map[string]int{},
		volumes:     map[string]map[string]string{},
		m:           &sync.Mutex{},
	}
}

func (d s3Driver) Create(r volume.Request) volume.Response {
	log.Printf("Creating volume %s\n", r.Name)
	d.m.Lock()
	defer d.m.Unlock()
	d.volumes[r.Name] = r.Options
	return volume.Response{}
}

func (d s3Driver) Get(r volume.Request) volume.Response {
	d.m.Lock()
	defer d.m.Unlock()
	if _, exists := d.volumes[r.Name]; exists {
		return volume.Response{
			Volume: &volume.Volume{
				Name:       r.Name,
				Mountpoint: d.mountpoint(r.Name),
			},
		}
	}
	return volume.Response{Err: fmt.Sprintf("Unable to find volume mounted on %s", d.mountpoint(r.Name))}
}

func (d s3Driver) List(r volume.Request) volume.Response {
	d.m.Lock()
	defer d.m.Unlock()
	var volumes []*volume.Volume
	for k := range d.volumes {
		volumes = append(volumes, &volume.Volume{
			Name:       k,
			Mountpoint: d.mountpoint(k),
		})
	}
	return volume.Response{
		Volumes: volumes,
	}
}

func (d s3Driver) Remove(r volume.Request) volume.Response {
	log.Printf("Removing volume %s\n", r.Name)
	d.m.Lock()
	defer d.m.Unlock()
	bucket := strings.SplitN(r.Name, "/", 2)[0]

	count, exists := d.connections[bucket]
	if exists && count < 1 {
		delete(d.connections, bucket)
	}
	delete(d.volumes, r.Name)
	return volume.Response{}
}

func (d s3Driver) Path(r volume.Request) volume.Response {
	return volume.Response{
		Mountpoint: d.mountpoint(r.Name),
	}
}

func (d s3Driver) Mount(r volume.MountRequest) volume.Response {
	d.m.Lock()
	defer d.m.Unlock()

	bucket := strings.SplitN(r.Name, "/", 2)[0]

	log.Printf("Mounting volume %s on %s\n", r.Name, d.mountpoint(bucket))

	count, exists := d.connections[bucket]
	if exists && count > 0 {
		d.connections[bucket] = count + 1
		return volume.Response{Mountpoint: d.mountpoint(r.Name)}
	}

	fi, err := os.Lstat(d.mountpoint(bucket))

	if os.IsNotExist(err) {
		if err := os.MkdirAll(d.mountpoint(bucket), 0755); err != nil {
			return volume.Response{Err: err.Error()}
		}
	} else if err != nil {
		if e, ok := err.(*os.PathError); ok && e.Err == syscall.ENOTCONN {
			// Crashed previously? Unmount
			fuse.Unmount(d.mountpoint(bucket))
		} else {
			return volume.Response{Err: err.Error()}
		}
	}

	if fi != nil && !fi.IsDir() {
		return volume.Response{Err: fmt.Sprintf("%v already exist and it's not a directory", d.mountpoint(bucket))}
	}

	err = d.mountBucket(bucket, r.Name)
	if err != nil {
		return volume.Response{Err: err.Error()}
	}

	d.connections[bucket] = 1

	return volume.Response{Mountpoint: d.mountpoint(r.Name)}
}

func (d s3Driver) Unmount(r volume.UnmountRequest) volume.Response {
	d.m.Lock()
	defer d.m.Unlock()

	bucket := strings.SplitN(r.Name, "/", 2)[0]

	log.Printf("Unmounting volume %s from %s\n", r.Name, d.mountpoint(bucket))

	if count, exists := d.connections[bucket]; exists {
		if count == 1 {
			mountpoint := d.mountpoint(bucket)
			fuse.Unmount(mountpoint)
			os.Remove(mountpoint)
		}
		d.connections[bucket] = count - 1
	} else {
		return volume.Response{Err: fmt.Sprintf("Unable to find volume mounted on %s", d.mountpoint(bucket))}
	}

	return volume.Response{}
}

func (d *s3Driver) mountpoint(name string) string {
	return filepath.Join(d.root, name)
}

func (d *s3Driver) mountBucket(name string, volumeName string) error {

	awsConfig := &aws.Config{
		DisableSSL:       aws.Bool(false),
		S3ForcePathStyle: aws.Bool(true),
		Region:           aws.String("us-east-1"),
	}
	goofysFlags := &goofys.FlagStorage{
		StorageClass: "STANDARD",
	}

	bucket := name
	if bkt, ok := d.volumes[volumeName]["bucket"]; ok {
		bucket = bkt
	}
	if prefix, ok := d.volumes[volumeName]["prefix"]; ok {
		bucket = bucket + ":" + prefix
	}
	if region, ok := d.volumes[volumeName]["region"]; ok {
		awsConfig.Region = aws.String(region)
	}
	if profile, ok := d.volumes[volumeName]["profile"]; ok {
		awsConfig.Credentials = credentials.NewSharedCredentials("", profile)
	}
	if endpoint, ok := d.volumes[volumeName]["endpoint"]; ok {
		awsConfig.Endpoint = aws.String(endpoint)
	}
	if storageClass, ok := d.volumes[volumeName]["storage-class"]; ok {
		goofysFlags.StorageClass = storageClass
	}
	if debugS3, ok := d.volumes[volumeName]["debugs3"]; ok {
		if s, err := strconv.ParseBool(debugS3); err == nil {
			goofysFlags.DebugS3 = s
		}
	}
	if dirmode, ok := d.volumes[volumeName]["dir-mode"]; ok {
		if i, err := strconv.ParseUint(dirmode,0,32); err == nil {
			goofysFlags.DirMode = os.FileMode(i)
		}
	}
	if filemode, ok := d.volumes[volumeName]["file-mode"]; ok {
		if i, err := strconv.ParseUint(filemode,0,32); err == nil {
			goofysFlags.FileMode = os.FileMode(i)
		}
	}
	if uid, ok := d.volumes[volumeName]["uid"]; ok {
		if i, err := strconv.ParseUint(uid,0,32); err == nil {
			goofysFlags.Uid = uint32(i)
		}
	}
	if gid, ok := d.volumes[volumeName]["gid"]; ok {
		if i, err := strconv.ParseUint(gid,0,32); err == nil {
			goofysFlags.Gid = uint32(i)
		}
	}
	log.Printf("Create Goofys for bucket %s\n", bucket)
	g := goofys.NewGoofys(bucket, awsConfig, goofysFlags)
	if g == nil {
		err := fmt.Errorf("Goofys: initialization failed")
		return err
	}
	server := fuseutil.NewFileSystemServer(g)

	mountCfg := &fuse.MountConfig{
		FSName:                  name,
		Options:                 map[string]string{"allow_other": ""},
		DisableWritebackCaching: true,
	}

	_, err := fuse.Mount(d.mountpoint(name), server, mountCfg)
	if err != nil {
		err = fmt.Errorf("Mount: %v", err)
		return err
	}

	return nil
}

func (d s3Driver) Capabilities(r volume.Request) volume.Response {
	log.Printf("Capabilities %+v\n", r)
	return volume.Response{
		Capabilities: volume.Capability{Scope: "local"},
	}
}