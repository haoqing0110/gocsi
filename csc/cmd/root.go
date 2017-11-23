package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"text/template"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/thecodeteam/gocsi"
)

var debug, _ = strconv.ParseBool(os.Getenv("X_CSI_DEBUG"))

var root struct {
	ctx       context.Context
	client    *grpc.ClientConn
	tpl       *template.Template
	userCreds map[string]string

	genMarkdown bool
	logLevel    logLevelArg
	format      string
	endpoint    string
	insecure    bool
	timeout     time.Duration
	version     csiVersionArg
	metadata    mapOfStringArg

	withReqLogging bool
	withRepLogging bool

	withSpecValidator                    bool
	withRequiresCreds                    bool
	withSuccessCreateVolumeAlreadyExists bool
	withSuccessDeleteVolumeNotFound      bool
	withRequiresNodeID                   bool
	withRequiresPubVolInfo               bool
	withRequiresVolumeAttributes         bool
}

var (
	activeArgs []string
	activeCmd  *cobra.Command
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "csc",
	Short: "a command line container storage interface (CSI) client",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {

		// Enable debug level logging and request and response logging
		// if the environment variable that controls deubg mode is set
		// to a truthy value.
		if debug {
			root.logLevel.Set(log.DebugLevel.String())
			root.withReqLogging = true
			root.withReqLogging = true
		}

		// Set the log level.
		lvl, _ := root.logLevel.Val()
		log.SetLevel(lvl)

		if debug {
			log.Warn("debug mode enabled")
		}

		root.ctx = context.Background()
		log.Debug("assigned the root context")

		// Initialize the template if necessary.
		if root.format == "" {
			switch cmd.Name() {
			case listVolumesCmd.Name():
				if listVolumes.paging {
					root.format = volumeInfoFormat
				} else {
					root.format = listVolumesFormat
				}
			case createVolumeCmd.Name():
				root.format = volumeInfoFormat
			case supportedVersCmd.Name():
				root.format = supportedVersionsFormat
			case pluginInfoCmd.Name():
				root.format = pluginInfoFormat
			}
		}
		if root.format != "" {
			tpl, err := template.New("t").Parse(root.format)
			if err != nil {
				return err
			}
			root.tpl = tpl
		}

		// Parse the credentials if they exist.
		root.userCreds = gocsi.ParseMap(os.Getenv("X_CSI_USER_CREDENTIALS"))

		// Create the gRPC client connection.
		opts := []grpc.DialOption{
			grpc.WithDialer(
				func(target string, timeout time.Duration) (net.Conn, error) {
					proto, addr, err := gocsi.ParseProtoAddr(target)
					if err != nil {
						return nil, err
					}
					return net.DialTimeout(proto, addr, timeout)
				}),
		}

		// Disable TLS if specified.
		if root.insecure {
			opts = append(opts, grpc.WithInsecure())
		}

		var iceptors []grpc.UnaryClientInterceptor

		// Configure logging.
		if root.withReqLogging || root.withRepLogging {

			// Automatically enable request ID injection if logging
			// is enabled.
			iceptors = append(iceptors,
				gocsi.NewClientRequestIDInjector())
			log.Debug("enabled request ID injector")

			var (
				loggingOpts []gocsi.LoggingOption
				w           = newLogger(log.Infof)
			)

			if root.withReqLogging {
				loggingOpts = append(loggingOpts, gocsi.WithRequestLogging(w))
				log.Debug("enabled request logging")
			}
			if root.withRepLogging {
				loggingOpts = append(loggingOpts, gocsi.WithResponseLogging(w))
				log.Debug("enabled response logging")
			}
			iceptors = append(iceptors,
				gocsi.NewClientLogger(loggingOpts...))
		}

		// Configure the spec validator.
		root.withSpecValidator = root.withSpecValidator ||
			root.withRequiresCreds ||
			root.withSuccessCreateVolumeAlreadyExists ||
			root.withSuccessDeleteVolumeNotFound ||
			root.withRequiresNodeID ||
			root.withRequiresPubVolInfo ||
			root.withRequiresVolumeAttributes
		if root.withSpecValidator {
			var specOpts []gocsi.SpecValidatorOption
			if root.withRequiresCreds {
				specOpts = append(specOpts,
					gocsi.WithRequiresCreateVolumeCredentials(),
					gocsi.WithRequiresDeleteVolumeCredentials(),
					gocsi.WithRequiresControllerPublishVolumeCredentials(),
					gocsi.WithRequiresControllerUnpublishVolumeCredentials(),
					gocsi.WithRequiresNodePublishVolumeCredentials(),
					gocsi.WithRequiresNodeUnpublishVolumeCredentials())
				log.Debug("enabled spec validator opt: requires creds")
			}
			if root.withRequiresNodeID {
				specOpts = append(specOpts,
					gocsi.WithRequiresNodeID())
				log.Debug("enabled spec validator opt: requires node ID")
			}
			if root.withRequiresPubVolInfo {
				specOpts = append(specOpts,
					gocsi.WithRequiresPublishVolumeInfo())
				log.Debug("enabled spec validator opt: requires pub vol info")
			}
			if root.withRequiresVolumeAttributes {
				specOpts = append(specOpts,
					gocsi.WithRequiresVolumeAttributes())
				log.Debug("enabled spec validator opt: requires vol attribs")
			}
			if root.withSuccessCreateVolumeAlreadyExists {
				specOpts = append(specOpts,
					gocsi.WithSuccessCreateVolumeAlreadyExists())
				log.Debug("enabled spec validator opt: create exists success")
			}
			if root.withSuccessDeleteVolumeNotFound {
				specOpts = append(specOpts,
					gocsi.WithSuccessDeleteVolumeNotFound())
				log.Debug("enabled spec validator opt: delete !exists success")
			}
			iceptors = append(iceptors,
				gocsi.NewClientSpecValidator(specOpts...))
		}

		// Add interceptors to the client if any are configured.
		if len(iceptors) > 0 {
			opts = append(opts,
				grpc.WithUnaryInterceptor(gocsi.ChainUnaryClient(iceptors...)))
		}

		ctx, cancel := context.WithTimeout(root.ctx, root.timeout)
		defer cancel()
		client, err := grpc.DialContext(ctx, root.endpoint, opts...)
		if err != nil {
			return err
		}
		root.client = client

		return nil
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintf(
			os.Stderr,
			"%v\n\nPlease use -h,--help for more information\n",
			err)
		os.Exit(1)
	}
}

func init() {
	setHelpAndUsage(RootCmd)

	flagLogLevel(
		RootCmd.PersistentFlags(),
		&root.logLevel,
		"warn")

	flagEndpoint(
		RootCmd.PersistentFlags(),
		&root.endpoint,
		os.Getenv("CSI_ENDPOINT"))

	flagTimeout(
		RootCmd.PersistentFlags(),
		&root.timeout,
		"1m")

	flagWithRequestLogging(
		RootCmd.PersistentFlags(),
		&root.withReqLogging,
		"false")

	flagWithResponseLogging(
		RootCmd.PersistentFlags(),
		&root.withRepLogging,
		"false")

	flagWithSpecValidation(
		RootCmd.PersistentFlags(),
		&root.withSpecValidator,
		"false")

	RootCmd.PersistentFlags().BoolVarP(
		&root.insecure,
		"insecure",
		"i",
		true,
		`Disables transport security for the client via the gRPC dial option
        WithInsecure (https://goo.gl/Y95SfW)`)

	RootCmd.PersistentFlags().VarP(
		&root.metadata,
		"metadata",
		"m",
		`Sets one or more key/value pairs to use as gRPC metadata sent with all
        RPCs. gRPC metadata is similar to HTTP headers. For example:

            --metadata key1=val1 --m key2=val2,key3=val3

            -m key1=val1,key2=val2 --metadata key3=val3

        Read more on gRPC metadata at https://goo.gl/iTci67`)

	RootCmd.PersistentFlags().VarP(
		&root.version,
		"version",
		"v",
		`The version sent with an RPC may be specified as MAJOR.MINOR.PATCH`)

}

type logger struct {
	f func(msg string, args ...interface{})
	w io.Writer
}

func newLogger(f func(msg string, args ...interface{})) *logger {
	l := &logger{f: f}
	r, w := io.Pipe()
	l.w = w
	go func() {
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			f(scan.Text())
		}
	}()
	return l
}

func (l *logger) Write(data []byte) (int, error) {
	return l.w.Write(data)
}