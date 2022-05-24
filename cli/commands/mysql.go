package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wundergraph/wundergraph/pkg/datasources/database"
)

// postgresCmd represents the postgres command
var mysqlCmd = &cobra.Command{
	Use:     "mysql",
	Short:   "Introspects a mysql database",
	Example: `wunderctl introspect mysql mysql://user:password@localhost:5432/database`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		databaseURL := args[0]
		introspectionSchema := fmt.Sprintf(`datasource db {
			provider = "mysql"
			url      = "%s"
		}`, databaseURL)
		prismaSchema, graphqlSDL, dmmf, err := database.IntrospectPrismaDatabase(introspectionSchema, log)
		if err != nil {
			return err
		}
		result := DatabaseIntrospectionResult{
			PrismaSchema:  prismaSchema,
			GraphQLSchema: graphqlSDL,
			Dmmf:          []byte(dmmf),
		}
		emitIntrospectionResult(result)
		return nil
	},
}

func init() {
	introspectCmd.AddCommand(mysqlCmd)
}
