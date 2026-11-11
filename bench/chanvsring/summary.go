package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// row is the median of every metric over the runs of one benchmark name, and how many of those runs kept up.
type row struct {
	Name       string
	Runs, Kept int
	Median     map[string]float64
}

// summarize reads benchmark lines written by line (other lines are skipped) and returns one row per name, in the order
// the names first appear.
func summarize(reader io.Reader) ([]row, error) {
	var order []string
	values := map[string]map[string][]float64{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "BenchmarkChanVsRing/") {
			continue
		}
		name := strings.TrimPrefix(fields[0], "BenchmarkChanVsRing/")
		if values[name] == nil {
			values[name] = map[string][]float64{}
			order = append(order, name)
		}
		for index := 2; index+1 < len(fields); index += 2 {
			value, err := strconv.ParseFloat(fields[index], 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, fields[index+1], err)
			}
			values[name][fields[index+1]] = append(values[name][fields[index+1]], value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	rows := make([]row, 0, len(order))
	for _, name := range order {
		each := row{Name: name, Median: map[string]float64{}}
		for unit, list := range values[name] {
			each.Median[unit] = median(list)
			each.Runs = max(each.Runs, len(list))
		}
		for _, kept := range values[name]["kept"] {
			if kept == 1 {
				each.Kept++
			}
		}
		rows = append(rows, each)
	}
	return rows, nil
}

func median(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// summaryUnits are the columns printSummary writes, in order.
var summaryUnits = []string{"msgs/s", "cores", "producer-cores", "consumer-cores", "runtime-cores", "cores-per-Mmsg",
	"p50-ns", "p99-ns", "p999-ns", "behind-msgs", "backlog-msgs"}

func printSummary(writer io.Writer, rows []row) {
	fmt.Fprintf(writer, "name\truns\tkept\t%s\n", strings.Join(summaryUnits, "\t"))
	for _, each := range rows {
		fmt.Fprintf(writer, "%s\t%d\t%d", each.Name, each.Runs, each.Kept)
		for _, unit := range summaryUnits {
			fmt.Fprintf(writer, "\t%.4g", each.Median[unit])
		}
		fmt.Fprintln(writer)
	}
}
