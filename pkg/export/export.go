package export

import (
	"context"
	"github.com/bakito/kubexporter/pkg/types"
	"github.com/olekukonko/tablewriter"
	"github.com/vbauerster/mpb/v5"
	"github.com/vbauerster/mpb/v5/decor"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// NewExporter create a new exporter
func NewExporter(config *types.Config) (Exporter, error) {
	err := config.Validate()
	if err != nil {
		return nil, err
	}

	return &exporter{
		config: config,
	}, nil
}

// Exporter interface
type Exporter interface {
	Export() error
}

func (e *exporter) Export() error {
	kubeconfig := filepath.Join(
		os.Getenv("HOME"), ".kube", "config",
	)
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}

	dcl, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}

	resources, err := e.listResources(dcl)
	if err != nil {
		return err
	}

	sort.SliceStable(resources, types.Sort(resources))

	var prog *mpb.Progress

	var mainBar *mpb.Bar
	if e.config.Progress {
		prog = mpb.New()
		mainBar = prog.AddBar(int64(len(resources)),
			mpb.PrependDecorators(
				// display our name with one space on the right
				decor.Name("Resources", decor.WC{W: len("Resources") + 1, C: decor.DidentRight}),
				decor.Elapsed(decor.ET_STYLE_GO),
			),
			mpb.AppendDecorators(
				decor.CurrentNoUnit(""),
				decor.Name("/"),
				decor.TotalNoUnit(""),
				decor.Name(" "),
				decor.Percentage(),
			),
		)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dcl))
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	var workers []*worker
	for i := 0; i < e.config.Worker; i++ {
		workers = append(workers, newWorker(i, e.config, mapper, client, prog, mainBar))
	}

	workerErrors, err := runExport(workers, resources)
	if err != nil {
		return err
	}

	if prog != nil {
		prog.Wait()
	}

	if e.config.Summary {
		e.printSummary(workerErrors, resources)
	}

	if e.config.Archive {
		err = e.tarGz()
	}
	return err
}

func (e *exporter) listResources(dcl *discovery.DiscoveryClient) ([]*types.GroupResource, error) {
	lists, err := dcl.ServerPreferredResources()
	if err != nil {
		return nil, err
	}

	var resources []*types.GroupResource

	for _, list := range lists {
		if len(list.APIResources) == 0 {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, resource := range list.APIResources {
			r := &types.GroupResource{
				APIGroup:        gv.Group,
				APIVersion:      gv.Version,
				APIGroupVersion: gv.String(),
				APIResource:     resource,
			}
			if len(resource.Verbs) == 0 || e.config.IsExcluded(r) || (!resource.Namespaced && e.config.Namespace != "") {
				continue
			}

			resources = append(resources, r)
		}
	}
	return resources, nil
}

func (e *exporter) printSummary(workerErrors int, resources []*types.GroupResource) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeaderLine(false)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	header := []string{"Group", "Version", "Kind", "Namespaces", "Instances", "Query Duration", "Export Duration"}
	if workerErrors > 0 {
		header = append(header, "Error")
	}
	table.SetHeader(header)
	start := time.Now()
	qd := start
	ed := start
	var inst int

	for _, r := range resources {
		table.Append(r.Report(workerErrors > 0))
		qd = qd.Add(r.QueryDuration)
		ed = ed.Add(r.ExportDuration)
		inst += r.Instances
	}
	total := "TOTAL"
	if e.config.Worker > 1 {
		total = "CUMULATED " + total
	}
	table.Append([]string{total, "", "", "", strconv.Itoa(inst), qd.Sub(start).String(), ed.Sub(start).String()})
	table.Render()
}

type exporter struct {
	config *types.Config
}

func newWorker(id int, config *types.Config, mapper meta.RESTMapper, client dynamic.Interface, prog *mpb.Progress, mainBar *mpb.Bar) *worker {

	w := &worker{
		id:               id + 1,
		mainBar:          mainBar,
		config:           config,
		mapper:           mapper,
		client:           client,
		elapsedDecorator: decor.NewElapsed(decor.ET_STYLE_GO, time.Now()),
	}

	w.recBar = prog.AddBar(1,
		mpb.PrependDecorators(
			w.decorator(),
		),
		mpb.AppendDecorators(
			decor.CurrentNoUnit(""),
			decor.Name("/"),
			decor.TotalNoUnit(""),
			decor.Name(" "),
			decor.Percentage(),
		),
	)
	return w
}

func (w *worker) function(wg *sync.WaitGroup, out chan *types.GroupResource) func(resource *types.GroupResource) {

	return func(res *types.GroupResource) {
		defer wg.Done()

		ctx := context.TODO()
		w.currentKind = res.GroupKind()
		w.elapsedDecorator = decor.NewElapsed(decor.ET_STYLE_GO, time.Now())

		if w.recBar != nil {
			w.recBar.SetCurrent(0)
			w.recBar.SetTotal(0, false)
		}
		start := time.Now()
		ul, err := w.list(ctx, res.APIGroup, res.APIVersion, res.APIResource.Kind)

		res.QueryDuration = time.Now().Sub(start)
		start = time.Now()

		if err != nil {
			if errors.IsNotFound(err) {
				res.Error = "Not Found"
			} else if errors.IsMethodNotSupported(err) {
				res.Error = "Not Allowed"
			} else {
				res.Error = "Error:" + err.Error()
			}
		} else {
			if w.recBar != nil {
				w.recBar.SetTotal(int64(len(ul.Items)), false)
			}
			for _, u := range ul.Items {
				w.config.Excluded.FilterFields(res, u)

				us := &u
				b, err := w.config.Marshal(us)
				if err != nil {
					res.Error = err.Error()
					continue
				}
				filename, err := w.config.FileName(res, us)
				if err != nil {
					res.Error = err.Error()
					continue
				}

				_ = os.MkdirAll(filepath.Dir(filename), os.ModePerm)
				err = ioutil.WriteFile(filename, b, 0664)
				if err != nil {
					res.Error = err.Error()
					continue
				}
				if w.recBar != nil {
					w.recBar.Increment()
				}
			}
		}
		if ul != nil {
			res.Instances = len(ul.Items)
		}
		res.ExportDuration = time.Now().Sub(start)

		if w.mainBar != nil {
			w.mainBar.Increment()
		}
		out <- res
	}
}
