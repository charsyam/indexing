package secondaryindex

import (
	"fmt"
	qc "github.com/couchbase/indexing/secondary/queryport/client"
	"github.com/couchbaselabs/query/expression"
	"github.com/couchbaselabs/query/parser/n1ql"
	tc "github.com/couchbase/indexing/secondary/tests/framework/common"
)

var ClusterManagerAddr = "localhost:9100"

func CreateSecondaryIndex(indexName, bucketName string, indexFields []string) {

	client := qc.NewClusterClient(ClusterManagerAddr)
	var secExprs []string
	
	for _, indexField := range indexFields {
		expr, err := n1ql.ParseExpression(indexField)
		if err != nil {
			fmt.Printf("Creating index %v. Error while parsing the expression (%v) : %v\n", indexName, indexField, err)
		}

		secExprs = append(secExprs, expression.NewStringer().Visit(expr))
	}
	
	using    := "lsm"
	exprType := "N1QL"
	partnExp := ""
	where    := ""
	isPrimary := false
	
	_, err := client.CreateIndex(indexName, bucketName, using, exprType, partnExp, where, secExprs, isPrimary)
	if err == nil {
		fmt.Printf("Creating the secondary index %v\n", indexName)
	} else {
		fmt.Println("Error occured:", err)
	}
}

func DropSecondaryIndex(indexName string) {
	fmt.Printf("Dropping the secondary index %v", indexName)
	client := qc.NewClusterClient(ClusterManagerAddr)
	infos, err := client.List()
	tc.HandleError(err, "Error while listing the secondary indexes")
	
	for _, info := range infos {
		if info.Name == indexName {
			e := client.DropIndex(info.DefnID)
			if e == nil {
				fmt.Println("Index dropped")
			} else {
				tc.HandleError(e, "Error dropping the index " + indexName)
			}
		}
	}
}

func DropAllSecondaryIndexes() {
	client := qc.NewClusterClient(ClusterManagerAddr)
	infos, err := client.List()
	tc.HandleError(err, "Error while listing the secondary indexes")

	for _, info := range infos {
		e := client.DropIndex(info.DefnID)
		tc.HandleError(e, "Error dropping the index " + info.Name)
	}
}
