#ifndef OPENMM_BENCHMARK_H_
#define OPENMM_BENCHMARK_H_

#include <OpenMM.h>

class OpenMMBenchmark {

public:

    OpenMMBenchmark(std::string platformName,
                    std::map<std::string, std::string> contextProperties = std::map<std::string, std::string>());

    ~OpenMMBenchmark();

    // return the speed
    virtual double speed();

private:

    int start_time;

    double time_elapsed;

    long long total_iterations;

    std::string platformName;

    OpenMM::Context *ctxt;

    OpenMM::Integrator *intg;

    OpenMM::State *initial_state;

    OpenMM::System *sys;

};

#endif