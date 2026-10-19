// Command s3poke lists, reads and overwrites objects in the Ant test buckets. It exists only to inject
// storage faults (bit-rot, missing object) for the AC5/AC6 integrity evidence.
//
//	s3poke <oke|kw> ls [prefix-under-ant/]
//	s3poke <oke|kw> get <key> <out-file>
//	s3poke <oke|kw> put <key> <in-file>
//	s3poke <oke|kw> rm  <key>
//
// Credentials come from ~/.kuvryn-ant/<ctx>-s3.json and are never printed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type target struct{ endpoint, region, ctx string }

var targets = map[string]target{
	"oke": {"https://axmcgpqk0w0z.compat.objectstorage.me-dubai-1.oraclecloud.com", "me-dubai-1", "azrty-oke"},
	"kw":  {"http://192.168.10.253:9000", "us-east-1", "kw"},
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: s3poke <oke|kw> ls|get|put|rm ...")
		os.Exit(2)
	}
	t, ok := targets[os.Args[1]]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown target")
		os.Exit(2)
	}
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".kuvryn-ant", t.ctx+"-s3.json"))
	check(err)
	var cred struct {
		ID     string `json:"access_key_id"`
		Secret string `json:"secret_access_key"`
	}
	check(json.Unmarshal(b, &cred))
	cl := s3.New(s3.Options{
		BaseEndpoint: aws.String(t.endpoint), Region: t.region, UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: cred.ID, SecretAccessKey: cred.Secret}, nil
		}),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	const bucket = "kuvryn-ant"
	ctx := context.Background()
	switch os.Args[2] {
	case "ls":
		prefix := "ant/"
		if len(os.Args) > 3 {
			prefix += os.Args[3]
		}
		p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
		for p.HasMorePages() {
			pg, err := p.NextPage(ctx)
			check(err)
			for _, o := range pg.Contents {
				fmt.Printf("%s\t%d\t%s\n", aws.ToString(o.Key), aws.ToInt64(o.Size), o.LastModified.UTC().Format("2006-01-02T15:04:05Z"))
			}
		}
	case "get":
		o, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(os.Args[3])})
		check(err)
		f, err := os.Create(os.Args[4])
		check(err)
		_, err = io.Copy(f, o.Body)
		check(err)
		check(f.Close())
	case "put":
		d, err := os.ReadFile(os.Args[4])
		check(err)
		_, err = cl.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(os.Args[3]), Body: bytes.NewReader(d)})
		check(err)
	case "presign": // presign <absolute-key>: print a 30-minute GET URL (used for the direct-backend throughput baseline; never commit the output)
		u, err := s3.NewPresignClient(cl).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(os.Args[3])}, s3.WithPresignExpires(30*time.Minute))
		check(err)
		fmt.Println(u.URL)
	case "rmprefix": // rmprefix <absolute-prefix>: delete every object under a scratch prefix (never "ant/")
		prefix := os.Args[3]
		if prefix == "" || strings.HasPrefix(prefix, "ant/") || !strings.HasSuffix(prefix, "/") {
			check(fmt.Errorf("refusing prefix %q", prefix))
		}
		n, bytes := 0, int64(0)
		p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
		for p.HasMorePages() {
			pg, err := p.NextPage(ctx)
			check(err)
			for _, o := range pg.Contents {
				_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key})
				check(err)
				n++
				bytes += aws.ToInt64(o.Size)
			}
		}
		fmt.Printf("deleted %d objects (%d bytes) under %s\n", n, bytes, prefix)
	case "rm":
		_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(os.Args[3])})
		check(err)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "s3poke:", err)
		os.Exit(1)
	}
}
