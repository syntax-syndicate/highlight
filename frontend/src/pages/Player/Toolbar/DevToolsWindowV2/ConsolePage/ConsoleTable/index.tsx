import { ApolloError } from '@apollo/client'
import { Button } from '@components/Button'
import { LogCustomColumn } from '@components/CustomColumnPopover'
import { Link } from '@components/Link'
import LoadingBox from '@components/LoadingBox'
import {
	Box,
	Callout,
	IconSolidCheveronDown,
	IconSolidCheveronRight,
	Stack,
	Table,
	Text,
} from '@highlight-run/ui/components'
import { ColumnRenderers } from '@pages/LogsPage/LogsTable/CustomColumns/renderers'
import { FullScreenContainer } from '@pages/LogsPage/LogsTable/FullScreenContainer'
import { NoLogsFound } from '@pages/LogsPage/LogsTable/NoLogsFound'
import { LogEdgeWithResources } from '@pages/LogsPage/useGetLogs'
import {
	ColumnDef,
	createColumnHelper,
	ExpandedState,
	flexRender,
	getCoreRowModel,
	getExpandedRowModel,
	Row,
	useReactTable,
} from '@tanstack/react-table'
import { useVirtualizer } from '@tanstack/react-virtual'
import clsx from 'clsx'
import _ from 'lodash'
import React, {
	Key,
	useCallback,
	useEffect,
	useMemo,
	useRef,
	useState,
} from 'react'

import { ColumnHeader } from '@/components/CustomColumnHeader'
import { findMatchingAttributes } from '@/components/JsonViewer/utils'
import { SearchExpression } from '@/components/Search/Parser/listener'
import { LogEdge } from '@/graph/generated/schemas'
import { LogDetails } from '@/pages/LogsPage/LogsTable/LogDetails'
import { THROTTLED_UPDATE_MS } from '@/pages/Player/PlayerHook/PlayerState'
import analytics from '@/util/analytics'

import * as styles from './style.css'

type Props = {
	loading: boolean
	error: ApolloError | undefined
	refetch: () => void
} & ConsoleTableInnerProps

export const ConsoleTable = (props: Props) => {
	if (props.loading) {
		return (
			<FullScreenContainer>
				<LoadingBox />
			</FullScreenContainer>
		)
	}

	if (props.error) {
		return (
			<FullScreenContainer>
				<Box m="auto" style={{ maxWidth: 300 }}>
					<Callout title="Failed to load logs" kind="error">
						<Box mb="6">
							<Text color="moderate">
								There was an error loading your logs. Reach out
								to us if this might be a bug.
							</Text>
						</Box>
						<Stack direction="row">
							<Button
								kind="secondary"
								trackingId="logs-error-reload"
								onClick={() => props.refetch()}
							>
								Reload query
							</Button>
							<Box
								display="flex"
								alignItems="center"
								justifyContent="center"
							>
								<Link
									to="https://highlight.io/community"
									target="_blank"
								>
									Help
								</Link>
							</Box>
						</Stack>
					</Callout>
				</Box>
			</FullScreenContainer>
		)
	}

	if (props.logEdges.length === 0) {
		return (
			<FullScreenContainer>
				<NoLogsFound />
			</FullScreenContainer>
		)
	}

	return <ConsoleTableInner {...props} />
}

type ConsoleTableInnerProps = {
	logEdges: LogEdgeWithResources[]
	selectedColumns: LogCustomColumn[]
	queryParts: SearchExpression[]
	lastActiveLogIndex: number
	autoScroll: boolean
	bodyHeight: string
}

const ConsoleTableInner = ({
	logEdges,
	selectedColumns,
	queryParts,
	lastActiveLogIndex,
	autoScroll,
	bodyHeight,
}: ConsoleTableInnerProps) => {
	const bodyRef = useRef<HTMLDivElement>(null)
	const [expanded, setExpanded] = useState<ExpandedState>({})

	const columnHelper = createColumnHelper<LogEdge>()

	const columnData = useMemo(() => {
		const gridColumns: string[] = []
		const columnHeaders: ColumnHeader[] = []
		const columns: ColumnDef<LogEdge, any>[] = []

		gridColumns.push('32px')
		columnHeaders.push({ id: 'cursor', component: '' })
		columns.push(
			columnHelper.accessor('cursor', {
				cell: ({ row }) => {
					return (
						<Table.Cell alignItems="flex-start">
							<Box flexShrink={0} display="flex" width="full">
								{row.getIsExpanded() ? (
									<IconExpanded />
								) : (
									<IconCollapsed />
								)}
							</Box>
						</Table.Cell>
					)
				},
			}),
		)

		selectedColumns.forEach((column) => {
			gridColumns.push(column.size)
			columnHeaders.push({
				id: column.id,
				component: column.label,
			})

			// @ts-ignore
			const accessor = columnHelper.accessor(`node.${column.accessKey}`, {
				cell: ({ row, getValue }) => {
					const ColumnRenderer =
						ColumnRenderers[column.type] || ColumnRenderers.string

					return (
						<ColumnRenderer
							row={row}
							getValue={getValue}
							queryParts={queryParts}
							onClick={column.onClick}
						/>
					)
				},
			})

			columns.push(accessor)
		})

		return {
			gridColumns,
			columnHeaders,
			columns,
		}
	}, [columnHelper, queryParts, selectedColumns])

	const table = useReactTable({
		data: logEdges,
		columns: columnData.columns,
		state: {
			expanded,
		},
		onExpandedChange: (expanded) => {
			setExpanded(expanded)

			if (expanded) {
				analytics.track('console_table-row-expand_click')
			} else {
				analytics.track('console_table-row-collapse_click')
			}
		},
		getRowCanExpand: (row) => row.original.node.logAttributes,
		getCoreRowModel: getCoreRowModel(),
		getExpandedRowModel: getExpandedRowModel(),
	})

	const { rows } = table.getRowModel()

	const rowVirtualizer = useVirtualizer({
		count: rows.length,
		estimateSize: () => 29,
		getScrollElement: () => bodyRef.current,
		overscan: 250,
	})

	const totalSize = rowVirtualizer.getTotalSize()
	const virtualRows = rowVirtualizer.getVirtualItems()
	const paddingTop = virtualRows.length > 0 ? virtualRows[0]?.start || 0 : 0
	const paddingBottom =
		virtualRows.length > 0
			? totalSize - (virtualRows[virtualRows.length - 1]?.end || 0)
			: 0

	useEffect(() => {
		table.toggleAllRowsExpanded(false)
	}, [table])

	// eslint-disable-next-line react-hooks/exhaustive-deps
	const scrollFunction = useCallback(
		_.debounce((index: number) => {
			requestAnimationFrame(() => {
				rowVirtualizer.scrollToIndex(index, {
					align: 'center',
					behavior: 'smooth',
				})
			})
		}, THROTTLED_UPDATE_MS),
		[],
	)

	useEffect(() => {
		if (logEdges) {
			if (autoScroll && lastActiveLogIndex >= 0) {
				scrollFunction(lastActiveLogIndex)
			}
		}
	}, [lastActiveLogIndex, logEdges, scrollFunction, autoScroll])

	return (
		<Table height="full" noBorder>
			<Table.Head>
				<Table.Row gridColumns={columnData.gridColumns}>
					{columnData.columnHeaders.map((header) => (
						<Table.Header
							key={header.id}
							noPadding={header.noPadding}
						>
							<Box
								display="flex"
								alignItems="center"
								justifyContent="space-between"
							>
								<Stack direction="row" gap="6" align="center">
									<Text lines="1">{header.component}</Text>
								</Stack>
							</Box>
						</Table.Header>
					))}
				</Table.Row>
			</Table.Head>
			<Table.Body
				ref={bodyRef}
				overflowY="auto"
				style={{ height: bodyHeight }}
				hiddenScroll
			>
				{paddingTop > 0 && <Box style={{ height: paddingTop }} />}
				{virtualRows.map((virtualRow) => {
					const row = rows[virtualRow.index]

					return (
						<ConsoleTableRow
							key={virtualRow.key}
							row={row}
							rowVirtualizer={rowVirtualizer}
							expanded={row.getIsExpanded()}
							virtualRowKey={virtualRow.key}
							gridColumns={columnData.gridColumns}
							queryParts={queryParts}
							lastActiveLogIndex={lastActiveLogIndex}
						/>
					)
				})}
				{paddingBottom > 0 && <Box style={{ height: paddingBottom }} />}
			</Table.Body>
		</Table>
	)
}

export const IconExpanded: React.FC = () => (
	<IconSolidCheveronDown color="#6F6E77" size="12" />
)

export const IconCollapsed: React.FC = () => (
	<IconSolidCheveronRight color="#6F6E77" size="12" />
)

type ConsoleTableRowProps = {
	row: Row<LogEdgeWithResources>
	rowVirtualizer: any
	expanded: boolean
	virtualRowKey: Key
	gridColumns: string[]
	queryParts: SearchExpression[]
	lastActiveLogIndex: number
}

const ConsoleTableRow: React.FC<ConsoleTableRowProps> = ({
	row,
	rowVirtualizer,
	expanded,
	virtualRowKey,
	queryParts,
	gridColumns,
	lastActiveLogIndex,
}) => {
	const attributesRow = (row: ConsoleTableRowProps['row']) => {
		const log = row.original.node
		const rowExpanded = row.getIsExpanded()

		return (
			<Table.Row
				selected={expanded}
				className={clsx(styles.attributesRow, {
					[styles.currentRow]: row.index === lastActiveLogIndex,
				})}
				gridColumns={['32px', '1fr']}
			>
				{rowExpanded && (
					<>
						<Table.Cell py="4" />
						<Table.Cell py="4" borderTop="dividerWeak">
							<LogDetails
								matchedAttributes={findMatchingAttributes(
									queryParts,
									{
										...log.logAttributes,
										environment: log.environment,
										level: log.level,
										message: log.message,
										secure_session_id: log.secureSessionID,
										service_name: log.serviceName,
										service_version: log.serviceVersion,
										source: log.source,
										span_id: log.spanID,
										trace_id: log.traceID,
									},
								)}
								row={row}
								queryParts={queryParts}
								hideRelatedResources
							/>
						</Table.Cell>
					</>
				)}
			</Table.Row>
		)
	}

	return (
		<div
			key={virtualRowKey}
			data-index={virtualRowKey}
			ref={rowVirtualizer.measureElement}
		>
			<Table.Row
				gridColumns={gridColumns}
				onClick={row.getToggleExpandedHandler()}
				selected={expanded}
				className={clsx(styles.dataRow, {
					[styles.pastRow]: row.index <= lastActiveLogIndex,
				})}
			>
				{row.getVisibleCells().map((cell: any) => {
					return (
						<React.Fragment key={cell.column.id}>
							{flexRender(
								cell.column.columnDef.cell,
								cell.getContext(),
							)}
						</React.Fragment>
					)
				})}
			</Table.Row>
			{attributesRow(row)}
		</div>
	)
}