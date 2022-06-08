package main

import (
	"fmt"
	"io/ioutil"
	"strconv"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-digitalocean/sdk/v4/go/digitalocean"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	sshKeyName     = "Redacted"
	privateKeyPath = "/redacted/redacted/.ssh/redacted"
	initFilePath   = "/etc/systemd/system/rocket.service"
)

func lookupDomain(ctx *pulumi.Context) (*digitalocean.LookupDomainResult, error) {
	var res, err = digitalocean.LookupDomain(ctx, &digitalocean.LookupDomainArgs{
		Name: "robbiemckinstry.tech",
	})
	return res, err
}

func getSSHKeyId(ctx *pulumi.Context) (string, error) {
	fmt.Println("Fetching SSH Key.")
	var sshLookupArgs = &digitalocean.LookupSshKeyArgs{
		Name: sshKeyName,
	}
	sshKey, err := digitalocean.LookupSshKey(ctx, sshLookupArgs, nil)
	if err != nil {
		return "", nil
	}
	var keyId = fmt.Sprintf("%d", sshKey.Id)
	return keyId, nil
}

func chainCommand(ctx *pulumi.Context, name, cmd string, conn remote.ConnectionInput, prior pulumi.Resource) (*remote.Command, error) {
	var deps = []pulumi.Resource{prior}
	var opts = []pulumi.ResourceOption{pulumi.DependsOn(deps)}
	var cmdResult, err = remote.NewCommand(ctx, name, &remote.CommandArgs{
		Connection: conn,
		Create:     pulumi.String(cmd),
	}, opts...)
	outputCmd(ctx, name, cmdResult)
	return cmdResult, err
}

func chainLocal(ctx *pulumi.Context, name, cmd string, prior pulumi.Resource) (*local.Command, error) {
	var deps = []pulumi.Resource{prior}
	var opts = []pulumi.ResourceOption{pulumi.DependsOn(deps)}
	var cmdResult, err = local.NewCommand(ctx, name, &local.CommandArgs{
		Create: pulumi.String(cmd),
	}, opts...)
	outputLocalCmd(ctx, name, cmdResult)
	return cmdResult, err
}

func outputCmd(ctx *pulumi.Context, name string, cmd *remote.Command) {
	var stdOutExport = fmt.Sprintf("%s-stdout", name)
	var stdErrExport = fmt.Sprintf("%s-stderr", name)
	ctx.Export(stdOutExport, cmd.Stdout)
	ctx.Export(stdErrExport, cmd.Stderr)
}

func outputLocalCmd(ctx *pulumi.Context, name string, cmd *local.Command) {
	var stdOutExport = fmt.Sprintf("%s-stdout", name)
	var stdErrExport = fmt.Sprintf("%s-stderr", name)
	ctx.Export(stdOutExport, cmd.Stdout)
	ctx.Export(stdErrExport, cmd.Stderr)
}

func registerSystemdManifest(ctx *pulumi.Context, conn remote.ConnectionInput, copyRes pulumi.Resource) error {

	var whichDocker, err = chainCommand(ctx, "where-is-docker", "which docker", conn, copyRes)
	if err != nil {
		return err
	}
	openFirewall, err := chainCommand(ctx, "open-firewall", "ufw allow 80", conn, whichDocker)
	if err != nil {
		return err
	}

	enableSystemd, err := chainCommand(ctx, "enable-systemd-manifest", "systemctl enable rocket.service", conn, openFirewall)
	if err != nil {
		return err
	}
	_, err = chainCommand(ctx, "start-systemd-manifest", "systemctl start rocket.service", conn, enableSystemd)
	if err != nil {
		return err
	}
	return err
}

func createDroplet(ctx *pulumi.Context, keyId string) (*digitalocean.Droplet, error) {
	fmt.Println("Creating Droplet.")
	return digitalocean.NewDroplet(ctx, "rust-web", &digitalocean.DropletArgs{
		Image:  pulumi.String("docker-20-04"),
		Region: pulumi.String("nyc3"),
		Size:   pulumi.String("s-1vcpu-1gb"),
		SshKeys: pulumi.StringArray{
			pulumi.String(keyId),
		},
	})
}

func openConnection(droplet *digitalocean.Droplet) (remote.ConnectionInput, error) {
	var dropletHostname = droplet.Ipv4Address
	var privateKey, err = ioutil.ReadFile(privateKeyPath)
	if err != nil {
		return nil, err
	}
	var conn = remote.ConnectionArgs{
		Host:       dropletHostname,
		User:       pulumi.String("root"),
		PrivateKey: pulumi.String(privateKey),
	}
	return conn, nil
}

func copySystemdManifest(ctx *pulumi.Context, conn remote.ConnectionInput, waitOn pulumi.Resource) (*remote.CopyFile, error) {
	fmt.Println("Copying Service file to droplet.")
	var sleepResult, err = chainLocal(ctx, "sleep", "sleep 30", waitOn)
	if err != nil {
		return nil, err
	}
	var deps = []pulumi.Resource{sleepResult}
	var opts = []pulumi.ResourceOption{pulumi.DependsOn(deps)}
	res, err := remote.NewCopyFile(ctx, "copy-systemd-file", &remote.CopyFileArgs{
		Connection: conn,
		LocalPath:  pulumi.String("rocket.service"),
		RemotePath: pulumi.String(initFilePath),
		Triggers:   nil,
	}, opts...)
	return res, err
}

func main() {

	// • Push container to container registry?
	// • Create a sample Service file (check Cacher for example)
	// • Copy file to Droplet.
	// • Exec remote commands to start the Service.
	pulumi.Run(func(ctx *pulumi.Context) error {
		// • Import my SSH Key from DigitalOcean
		//   so I can copy files to the Droplet.
		var keyId, err = getSSHKeyId(ctx)
		if err != nil {
			return err
		}
		// • Grab the domain so I can add a new DNS record.
		domain, err := lookupDomain(ctx)
		if err != nil {
			return err
		}

		// • Create the Droplet itself, assigning my ssh key.
		droplet, err := createDroplet(ctx, keyId)
		if err != nil {
			return err
		}

		// • Create a Let's Encrypt certificate
		cert, err := digitalocean.NewCertificate(ctx, "cert", &digitalocean.CertificateArgs{
			Domains: pulumi.StringArray{
				pulumi.String("pulumi.robbiemckinstry.tech"),
			},
			Type: pulumi.String("lets_encrypt"),
		})
		if err != nil {
			return err
		}

		// • Throw together a load balancer for the new droplet.
		var conversionCallback = func(val string) (int, error) {
			return strconv.Atoi(val)
		}
		var dropletId = droplet.ID().ToStringOutput().ApplyT(conversionCallback).(pulumi.IntOutput)
		lb, err := digitalocean.NewLoadBalancer(ctx, "rocket-lb", &digitalocean.LoadBalancerArgs{
			Region:                       pulumi.String("nyc3"),
			Name:                         pulumi.String("rocket-lb"),
			RedirectHttpToHttps:          pulumi.BoolPtr(true),
			DisableLetsEncryptDnsRecords: pulumi.BoolPtr(true),
			ForwardingRules: digitalocean.LoadBalancerForwardingRuleArray{
				&digitalocean.LoadBalancerForwardingRuleArgs{
					EntryPort:      pulumi.Int(80),
					EntryProtocol:  pulumi.String("http"),
					TargetPort:     pulumi.Int(80),
					TargetProtocol: pulumi.String("http"),
				},
				&digitalocean.LoadBalancerForwardingRuleArgs{
					CertificateName: cert.Name,
					EntryPort:       pulumi.Int(443),
					EntryProtocol:   pulumi.String("https"),
					TargetPort:      pulumi.Int(80),
					TargetProtocol:  pulumi.String("http"),
				},
			},
			DropletIds: pulumi.IntArray{
				dropletId,
			},
		})
		if err != nil {
			return err
		}
		ctx.Export("address", droplet.Ipv4Address)
		ctx.Export("lb-address", lb.Ip)
		ctx.Export("url", pulumi.String("https://pulumi.robbiemckinstry.tech"))

		// • Create a new DNS record at "pulumi.robbiemckinstry.tech"
		_, err = digitalocean.NewDnsRecord(ctx, "pulumi-dns", &digitalocean.DnsRecordArgs{
			Domain: pulumi.String(domain.Id),
			Name:   pulumi.String("pulumi"),
			Type:   pulumi.String("A"),
			Value:  lb.Ip,
		})
		if err != nil {
			return err
		}

		// • Create the connection details using provided creds.
		conn, err := openConnection(droplet)
		if err != nil {
			return err
		}
		// • Copy over the Systemd manifest.
		copyOutput, err := copySystemdManifest(ctx, conn, droplet)
		if err != nil {
			return err
		}
		// • Register the manifest with Systemd and launch it.
		err = registerSystemdManifest(ctx, conn, copyOutput)
		if err != nil {
			return err
		}
		return nil
	})
}
