import Table from "@material-ui/core/Table"
import TableBody from "@material-ui/core/TableBody"
import TableCell from "@material-ui/core/TableCell"
import TableContainer from "@material-ui/core/TableContainer"
import TableHead from "@material-ui/core/TableHead"
import TableRow from "@material-ui/core/TableRow"
import { FC } from "react"
import { WorkspaceItemMachineRef } from "../../xServices/workspaces/workspacesXService"
import { WorkspacesTableBody } from "./WorkspacesTableBody"

const Language = {
  name: "Name",
  template: "Template",
  lastUsed: "Last Used",
  version: "Version",
  status: "Status",
  lastBuiltBy: "Last Built By",
}

export interface WorkspacesTableProps {
  isLoading?: boolean
  workspaceRefs?: WorkspaceItemMachineRef[]
  filter?: string
  isNonInitialPage: boolean
}

export const WorkspacesTable: FC<
  React.PropsWithChildren<WorkspacesTableProps>
> = ({ isLoading, workspaceRefs, filter, isNonInitialPage }) => {
  return (
    <TableContainer>
      <Table>
        <TableHead>
          <TableRow>
            <TableCell width="25%">{Language.name}</TableCell>
            <TableCell width="35%">{Language.template}</TableCell>
            <TableCell width="20%">{Language.lastUsed}</TableCell>
            <TableCell width="20%">{Language.version}</TableCell>
            <TableCell width="20%">{Language.status}</TableCell>
            <TableCell width="1%"></TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          <WorkspacesTableBody
            isLoading={isLoading}
            workspaceRefs={workspaceRefs}
            filter={filter}
            isNonInitialPage={isNonInitialPage}
          />
        </TableBody>
      </Table>
    </TableContainer>
  )
}
