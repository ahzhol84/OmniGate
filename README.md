🛠 Technical Implementation
Concurrency Model
OmniGate leverages Go's "Share memory by communicating" philosophy. Each device worker runs in its own goroutine, pushing data into a unified or prioritized pipeline.

Backpressure Handling
To prevent system crashes during I/O spikes, the middleware implements a non-blocking select on the data channels:

📦 Getting Started
Prerequisites
Go 1.20+

Access to a database (MySQL/PostgreSQL) as configured in config.yaml

Installation
Clone the repository:

Build the server:

Run the middleware:

📄 License
This project is licensed under the MIT License - see the LICENSE file for details.
